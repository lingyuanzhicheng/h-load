package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"h-load/internal/config"
	"h-load/internal/httpclient"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	app_errors "h-load/internal/errors"
	"h-load/internal/models"

	"gorm.io/gorm"
)

type SearchAccountService struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	clientManager   *httpclient.HTTPClientManager
}

func NewSearchAccountService(db *gorm.DB, settingsManager *config.SystemSettingsManager, clientManager *httpclient.HTTPClientManager) *SearchAccountService {
	return &SearchAccountService{db: db, settingsManager: settingsManager, clientManager: clientManager}
}

func (s *SearchAccountService) httpClient(timeout time.Duration) *http.Client {
	settings := s.settingsManager.GetSettings()
	return s.clientManager.GetClient(&httpclient.Config{
		ConnectTimeout:        time.Duration(settings.ConnectTimeout) * time.Second,
		RequestTimeout:        timeout,
		IdleConnTimeout:       time.Duration(settings.IdleConnTimeout) * time.Second,
		MaxIdleConns:          settings.MaxIdleConns,
		MaxIdleConnsPerHost:   settings.MaxIdleConnsPerHost,
		ResponseHeaderTimeout: time.Duration(settings.ResponseHeaderTimeout) * time.Second,
		ProxyURL:              settings.ProxyURL,
		DisableCompression:    false,
		WriteBufferSize:       32 * 1024,
		ReadBufferSize:        32 * 1024,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	})
}

func (s *SearchAccountService) List(ctx context.Context, accountType, status string) ([]models.GitHubSearchAccount, error) {
	var accounts []models.GitHubSearchAccount
	query := s.db.WithContext(ctx).Order("created_at desc")
	if accountType != "" {
		query = query.Where("type = ?", accountType)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if err := query.Find(&accounts).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return accounts, nil
}

func (s *SearchAccountService) Create(ctx context.Context, account *models.GitHubSearchAccount) (*models.GitHubSearchAccount, error) {
	account.Type = strings.TrimSpace(account.Type)
	account.Credential = strings.TrimSpace(account.Credential)
	account.DeviceID = strings.TrimSpace(account.DeviceID)
	if account.Status == "" {
		account.Status = models.SearchAccountStatusActive
	}
	if err := validateSearchAccount(account); err != nil {
		return nil, err
	}
	if username, err := s.fetchUsername(ctx, account, 15*time.Second); err == nil {
		account.Username = username
	}
	if err := s.db.WithContext(ctx).Create(account).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return account, nil
}

func (s *SearchAccountService) Update(ctx context.Context, id uint, updates map[string]any) (*models.GitHubSearchAccount, error) {
	var account models.GitHubSearchAccount
	if err := s.db.WithContext(ctx).First(&account, id).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}

	allowed := map[string]bool{"device_id": true}
	filtered := make(map[string]any)
	for key, val := range updates {
		if allowed[key] {
			filtered[key] = val
		}
	}
	if len(filtered) == 0 {
		return &account, nil
	}
	if _, ok := filtered["device_id"]; ok {
		account.DeviceID = strings.TrimSpace(fmt.Sprint(filtered["device_id"]))
		if account.DeviceID == "" {
			return nil, NewI18nError(app_errors.ErrValidation, "validation.required_fields_missing", nil)
		}
		filtered["device_id"] = account.DeviceID
	}
	if _, deviceChanged := filtered["device_id"]; deviceChanged {
		if username, err := s.fetchUsername(ctx, &account, 15*time.Second); err == nil {
			filtered["username"] = username
		}
	}
	if err := s.db.WithContext(ctx).Model(&account).Updates(filtered).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	if err := s.db.WithContext(ctx).First(&account, id).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return &account, nil
}

func (s *SearchAccountService) Delete(ctx context.Context, id uint) error {
	var configs []models.GroupLeakScanConfig
	if err := s.db.WithContext(ctx).Find(&configs).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	for _, cfg := range configs {
		var accountIDs []uint
		if len(cfg.AccountIDs) > 0 && json.Unmarshal(cfg.AccountIDs, &accountIDs) == nil {
			for _, accountID := range accountIDs {
				if accountID == id {
					return NewI18nError(app_errors.ErrValidation, "validation.account_in_use", nil)
				}
			}
		}
	}
	if err := s.db.WithContext(ctx).Delete(&models.GitHubSearchAccount{}, id).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	return nil
}

// ClearByStatus 批量删除指定状态的账户，被泄露扫描配置引用的账户会被跳过。
func (s *SearchAccountService) ClearByStatus(ctx context.Context, status string) (int64, error) {
	var accounts []models.GitHubSearchAccount
	if err := s.db.WithContext(ctx).Where("status = ?", status).Find(&accounts).Error; err != nil {
		return 0, app_errors.ParseDBError(err)
	}

	// 获取所有泄露扫描配置中引用的账户ID
	var configs []models.GroupLeakScanConfig
	if err := s.db.WithContext(ctx).Find(&configs).Error; err != nil {
		return 0, app_errors.ParseDBError(err)
	}
	referencedIDs := map[uint]bool{}
	for _, cfg := range configs {
		var accountIDs []uint
		if len(cfg.AccountIDs) > 0 && json.Unmarshal(cfg.AccountIDs, &accountIDs) == nil {
			for _, id := range accountIDs {
				referencedIDs[id] = true
			}
		}
	}

	var deleted int64
	for _, account := range accounts {
		if referencedIDs[account.ID] {
			continue
		}
		if err := s.db.WithContext(ctx).Delete(&models.GitHubSearchAccount{}, account.ID).Error; err != nil {
			return deleted, app_errors.ParseDBError(err)
		}
		deleted++
	}
	return deleted, nil
}

func (s *SearchAccountService) MarkUsed(ctx context.Context, id uint, success bool) {
	s.MarkUsedWithRateLimit(ctx, id, success, false)
}

func (s *SearchAccountService) MarkUsedWithRateLimit(ctx context.Context, id uint, success bool, rateLimited bool) {
	now := time.Now()
	updates := map[string]any{"last_used_at": &now, "request_count": gorm.Expr("request_count + ?", 1)}
	if !success && !rateLimited {
		updates["failure_count"] = gorm.Expr("failure_count + ?", 1)
	}
	_ = s.db.WithContext(ctx).Model(&models.GitHubSearchAccount{}).Where("id = ?", id).Updates(updates).Error
}

// MarkRateLimited 将账户标记为受限状态（429）
func (s *SearchAccountService) MarkRateLimited(ctx context.Context, id uint) {
	_ = s.db.WithContext(ctx).Model(&models.GitHubSearchAccount{}).Where("id = ?", id).Update("status", models.SearchAccountStatusLimited).Error
}

func (s *SearchAccountService) Validate(ctx context.Context, id uint) (*models.GitHubSearchAccount, error) {
	var account models.GitHubSearchAccount
	if err := s.db.WithContext(ctx).First(&account, id).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}

	settings := s.settingsManager.GetSettings()
	timeout := time.Duration(settings.AccountRequestTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	username, validateErr := s.fetchUsername(ctx, &account, timeout)
	if validateErr == nil {
		validateErr = s.validateSearchAccess(ctx, &account, timeout)
	}

	updates := map[string]any{}
	if username != "" {
		updates["username"] = username
	}

	isRateLimited := validateErr != nil && errors.Is(validateErr, app_errors.ErrAccountRateLimited)

	switch {
	case validateErr == nil:
		// 验证成功：置为 active，重置 failure_count
		updates["status"] = models.SearchAccountStatusActive
		updates["failure_count"] = int64(0)
	case isRateLimited:
		// 429：置为 limited，不递增 failure_count
		updates["status"] = models.SearchAccountStatusLimited
	default:
		// 非 429 错误：递增 failure_count，按黑名单阈值判断是否置为 inactive
		newFailureCount := account.FailureCount + 1
		updates["failure_count"] = newFailureCount
		threshold := settings.AccountBlacklistThreshold
		if threshold > 0 && newFailureCount >= int64(threshold) {
			updates["status"] = models.SearchAccountStatusInactive
		}
	}

	if err := s.db.WithContext(ctx).Model(&account).Updates(updates).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	account.Status = updates["status"].(string)
	account.Username = username
	if fc, ok := updates["failure_count"]; ok {
		account.FailureCount = fc.(int64)
	}
	return &account, nil
}

func (s *SearchAccountService) ValidateMany(ctx context.Context, accountType, status string) (int, int, error) {
	accounts, err := s.List(ctx, accountType, status)
	if err != nil {
		return 0, 0, err
	}
	validCount := 0
	invalidCount := 0
	for _, account := range accounts {
		updated, err := s.Validate(ctx, account.ID)
		if err != nil {
			invalidCount++
			continue
		}
		if updated.Status == models.SearchAccountStatusActive {
			validCount++
		} else {
			invalidCount++
		}
	}
	return validCount, invalidCount, nil
}

func (s *SearchAccountService) fetchUsername(ctx context.Context, account *models.GitHubSearchAccount, timeout time.Duration) (string, error) {
	endpoint := "https://github.com/settings/profile"
	if account.Type == models.SearchAccountTypeGitHubAPI {
		endpoint = "https://api.github.com/user"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	if account.Type == models.SearchAccountTypeGitHubAPI {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+account.Credential)
	} else {
		req.Header.Set("Cookie", "user_session="+account.Credential)
	}
	resp, err := s.httpClient(timeout).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: [status 429]", app_errors.ErrAccountRateLimited)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github account validation failed: status %d", resp.StatusCode)
	}
	if account.Type == models.SearchAccountTypeGitHubWeb && (resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Host != "github.com" || resp.Request.URL.Path != "/settings/profile") {
		return "", fmt.Errorf("github web account validation failed: not authenticated")
	}
	if account.Type == models.SearchAccountTypeGitHubAPI {
		var payload struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", err
		}
		username := strings.TrimSpace(payload.Login)
		if username == "" {
			return "", fmt.Errorf("github api account validation failed: empty login")
		}
		return username, nil
	}
	username := extractGitHubWebUsername(string(body))
	if username == "" {
		return "", fmt.Errorf("github web account validation failed: empty login")
	}
	return username, nil
}

func (s *SearchAccountService) validateSearchAccess(ctx context.Context, account *models.GitHubSearchAccount, timeout time.Duration) error {
	query := "filename:README.md"
	encodedQuery := url.QueryEscape(query)
	endpoint := "https://github.com/search/blackbird_count?saved_searches=^&q=" + encodedQuery
	if account.Type == models.SearchAccountTypeGitHubAPI {
		endpoint = "https://api.github.com/search/code?q=" + encodedQuery + "&per_page=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	if account.Type == models.SearchAccountTypeGitHubAPI {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+account.Credential)
	} else {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Referer", "https://github.com/search?q="+encodedQuery+"^&type=code")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Cookie", "user_session="+account.Credential)
	}

	resp, err := s.httpClient(timeout).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: [status 429]", app_errors.ErrAccountRateLimited)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github search validation failed: status %d", resp.StatusCode)
	}
	if account.Type == models.SearchAccountTypeGitHubAPI {
		var payload struct {
			TotalCount *int   `json:"total_count"`
			Message    string `json:"message"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return err
		}
		if payload.Message != "" {
			return fmt.Errorf("github api search validation failed: %s", payload.Message)
		}
		if payload.TotalCount == nil {
			return fmt.Errorf("github api search validation failed: missing total_count")
		}
		return nil
	}
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Host != "github.com" || resp.Request.URL.Path != "/search/blackbird_count" {
		return fmt.Errorf("github web search validation failed: not authenticated")
	}
	var payload struct {
		Failed bool   `json:"failed"`
		Count  *int64 `json:"count"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	if payload.Failed || payload.Count == nil {
		if payload.Error != "" {
			return fmt.Errorf("github web search validation failed: %s", payload.Error)
		}
		return fmt.Errorf("github web search validation failed")
	}
	return nil
}

func extractGitHubWebUsername(content string) string {
	patterns := []string{
		`name="user-login"\s+content="([^"]+)"`,
		`name="octolytics-actor-login"\s+content="([^"]+)"`,
		`"viewer"\s*:\s*\{[^}]*"login"\s*:\s*"([^"]+)"`,
	}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		match := re.FindStringSubmatch(content)
		if len(match) > 1 {
			return strings.TrimSpace(match[1])
		}
	}
	return ""
}

func validateSearchAccount(account *models.GitHubSearchAccount) error {
	if account.Type != models.SearchAccountTypeGitHubAPI && account.Type != models.SearchAccountTypeGitHubWeb {
		return NewI18nError(app_errors.ErrValidation, "validation.invalid_account_type", nil)
	}
	if account.Status != models.SearchAccountStatusActive && account.Status != models.SearchAccountStatusInactive && account.Status != models.SearchAccountStatusLimited {
		return NewI18nError(app_errors.ErrValidation, "validation.invalid_status_value", nil)
	}
	if account.Credential == "" || account.DeviceID == "" {
		return NewI18nError(app_errors.ErrValidation, "validation.required_fields_missing", nil)
	}
	return nil
}
