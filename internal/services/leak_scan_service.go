package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"h-load/internal/config"
	"h-load/internal/httpclient"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	app_errors "h-load/internal/errors"
	"h-load/internal/models"

	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const githubSearchPerPage = 20
const githubAPIDefaultMaxPages = 10
const githubWebDefaultMaxPages = 5
const githubMaxRefinedQueries = 200

type leakSearchResult struct {
	URL  string `json:"url"`
	Line int    `json:"line,omitempty"`
}

type leakCandidate struct {
	Value string `json:"-"`
	URL   string `json:"url,omitempty"`
	Line  int    `json:"line,omitempty"`
}

type LeakScanConfigPayload struct {
	Enabled         bool     `json:"enabled"`
	SourceTypes     []string `json:"source_types"`
	AccountStrategy string   `json:"account_strategy"`
	AccountIDs      []uint   `json:"account_ids"`
	MaxPages        int      `json:"max_pages"`
	DeepIndex       bool     `json:"deep_index"`
	SearchRules     []string `json:"search_rules"`
	MatchRules      []string `json:"match_rules"`
}

type LeakScanStatusResponse struct {
	Config *models.GroupLeakScanConfig `json:"config"`
	Run    *models.GroupLeakScanRun    `json:"run"`
}

type LeakScanEventsResponse struct {
	Run        *models.GroupLeakScanRun    `json:"run"`
	Events     []models.GroupLeakScanEvent `json:"events"`
	Pagination map[string]any              `json:"pagination"`
}

type GroupLeakScanService struct {
	db                   *gorm.DB
	keyService           *KeyService
	searchAccountService *SearchAccountService
	groupManager         *GroupManager
	settingsManager      *config.SystemSettingsManager
	clientManager        *httpclient.HTTPClientManager
	mu                   sync.Mutex
	cancelByGroup        map[uint]context.CancelFunc
}

func NewGroupLeakScanService(db *gorm.DB, keyService *KeyService, searchAccountService *SearchAccountService, groupManager *GroupManager, settingsManager *config.SystemSettingsManager, clientManager *httpclient.HTTPClientManager) *GroupLeakScanService {
	return &GroupLeakScanService{
		db:                   db,
		keyService:           keyService,
		searchAccountService: searchAccountService,
		groupManager:         groupManager,
		settingsManager:      settingsManager,
		clientManager:        clientManager,
		cancelByGroup:        make(map[uint]context.CancelFunc),
	}
}

func (s *GroupLeakScanService) httpClient(timeout time.Duration) *http.Client {
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

func (s *GroupLeakScanService) GetConfig(ctx context.Context, groupID uint) (*models.GroupLeakScanConfig, error) {
	var cfg models.GroupLeakScanConfig
	result := s.db.WithContext(ctx).Where("group_id = ?", groupID).Limit(1).Find(&cfg)
	if result.Error != nil {
		return nil, app_errors.ParseDBError(result.Error)
	}
	if result.RowsAffected > 0 {
		return &cfg, nil
	}

	blank, _ := json.Marshal([]string{})
	ids, _ := json.Marshal([]uint{})
	return &models.GroupLeakScanConfig{
		GroupID:         groupID,
		Enabled:         false,
		SourceTypes:     datatypes.JSON(blank),
		AccountStrategy: models.LeakScanAccountStrategyRoundRobin,
		AccountIDs:      datatypes.JSON(ids),
		MaxPages:        0,
		DeepIndex:       false,
		SearchRules:     datatypes.JSON(blank),
		MatchRules:      datatypes.JSON(blank),
	}, nil
}

func (s *GroupLeakScanService) SaveConfig(ctx context.Context, groupID uint, payload LeakScanConfigPayload) (*models.GroupLeakScanConfig, error) {
	if err := s.validateConfig(ctx, groupID, payload); err != nil {
		return nil, err
	}
	sourceTypes, _ := json.Marshal(payload.SourceTypes)
	accountIDs, _ := json.Marshal(payload.AccountIDs)
	searchRules, _ := json.Marshal(payload.SearchRules)
	matchRules, _ := json.Marshal(payload.MatchRules)

	cfg := models.GroupLeakScanConfig{
		GroupID:         groupID,
		Enabled:         payload.Enabled,
		SourceTypes:     datatypes.JSON(sourceTypes),
		AccountStrategy: payload.AccountStrategy,
		AccountIDs:      datatypes.JSON(accountIDs),
		MaxPages:        payload.MaxPages,
		DeepIndex:       payload.DeepIndex,
		SearchRules:     datatypes.JSON(searchRules),
		MatchRules:      datatypes.JSON(matchRules),
	}

	var existing models.GroupLeakScanConfig
	result := s.db.WithContext(ctx).Where("group_id = ?", groupID).Limit(1).Find(&existing)
	if result.Error != nil {
		return nil, app_errors.ParseDBError(result.Error)
	}
	if result.RowsAffected == 0 {
		if err := s.db.WithContext(ctx).Create(&cfg).Error; err != nil {
			return nil, app_errors.ParseDBError(err)
		}
		return &cfg, nil
	}

	updates := map[string]any{
		"enabled":          cfg.Enabled,
		"source_types":     cfg.SourceTypes,
		"account_strategy": cfg.AccountStrategy,
		"account_ids":      cfg.AccountIDs,
		"max_pages":        cfg.MaxPages,
		"deep_index":       cfg.DeepIndex,
		"search_rules":     cfg.SearchRules,
		"match_rules":      cfg.MatchRules,
	}
	if err := s.db.WithContext(ctx).Model(&existing).Updates(updates).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return s.GetConfig(ctx, groupID)
}

func (s *GroupLeakScanService) GetStatus(ctx context.Context, groupID uint) (*LeakScanStatusResponse, error) {
	cfg, err := s.GetConfig(ctx, groupID)
	if err != nil {
		return nil, err
	}
	var run models.GroupLeakScanRun
	result := s.db.WithContext(ctx).Where("group_id = ?", groupID).Order("created_at desc").Limit(1).Find(&run)
	if result.Error != nil {
		return nil, app_errors.ParseDBError(result.Error)
	}
	if result.RowsAffected == 0 {
		return &LeakScanStatusResponse{Config: cfg}, nil
	}
	return &LeakScanStatusResponse{Config: cfg, Run: &run}, nil
}

func (s *GroupLeakScanService) Start(ctx context.Context, groupID uint) (*models.GroupLeakScanRun, error) {
	return s.startWithCleanup(ctx, groupID, true)
}

func (s *GroupLeakScanService) Resume(ctx context.Context, groupID uint) (*models.GroupLeakScanRun, error) {
	return s.startWithCleanup(ctx, groupID, false)
}

func (s *GroupLeakScanService) startWithCleanup(ctx context.Context, groupID uint, clearOld bool) (*models.GroupLeakScanRun, error) {
	cfg, err := s.GetConfig(ctx, groupID)
	if err != nil {
		return nil, err
	}
	payload, err := configToPayload(cfg)
	if err != nil {
		return nil, err
	}
	if err := s.validateConfig(ctx, groupID, payload); err != nil {
		return nil, err
	}
	if !payload.Enabled {
		return nil, fmt.Errorf("leak scan is disabled")
	}

	s.mu.Lock()
	if _, exists := s.cancelByGroup[groupID]; exists {
		s.mu.Unlock()
		return nil, NewI18nError(app_errors.ErrValidation, "validation.task_already_running", nil)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancelByGroup[groupID] = cancel
	s.mu.Unlock()

	if clearOld {
		s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanEvent{})
		s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanRun{})
	}

	now := time.Now()
	run := models.GroupLeakScanRun{GroupID: groupID, Status: models.LeakScanStatusRunning, StartedAt: &now}
	if err := s.db.WithContext(ctx).Create(&run).Error; err != nil {
		s.clearCancel(groupID)
		return nil, app_errors.ParseDBError(err)
	}
	logrus.WithFields(logrus.Fields{"run_id": run.ID, "group_id": groupID}).Info("leak scan run created")
	_ = s.logEvent(ctx, run.ID, groupID, "run_created", "info", "泄露扫描任务已创建", nil)

	go s.run(runCtx, run.ID, groupID)
	return &run, nil
}

func (s *GroupLeakScanService) Stop(ctx context.Context, groupID uint) error {
	s.mu.Lock()
	cancel, exists := s.cancelByGroup[groupID]
	if exists {
		cancel()
		delete(s.cancelByGroup, groupID)
	}
	s.mu.Unlock()
	if exists {
		now := time.Now()
		return s.db.WithContext(ctx).Model(&models.GroupLeakScanRun{}).
			Where("group_id = ? AND status = ?", groupID, models.LeakScanStatusRunning).
			Updates(map[string]any{"status": models.LeakScanStatusInterrupted, "finished_at": &now}).Error
	}
	return nil
}

func (s *GroupLeakScanService) Reset(ctx context.Context, groupID uint) (*models.GroupLeakScanRun, error) {
	_ = s.Stop(ctx, groupID)
	return s.Start(ctx, groupID)
}

func (s *GroupLeakScanService) ListRuns(ctx context.Context, groupID uint, page, pageSize int) ([]models.GroupLeakScanRun, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	query := s.db.WithContext(ctx).Model(&models.GroupLeakScanRun{}).Where("group_id = ?", groupID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, app_errors.ParseDBError(err)
	}
	var runs []models.GroupLeakScanRun
	if err := query.Order("created_at desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&runs).Error; err != nil {
		return nil, 0, app_errors.ParseDBError(err)
	}
	return runs, total, nil
}

func (s *GroupLeakScanService) ListEvents(ctx context.Context, groupID uint, runID uint, page, pageSize int) (*LeakScanEventsResponse, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	var run models.GroupLeakScanRun
	queryRun := s.db.WithContext(ctx).Where("group_id = ?", groupID)
	if runID > 0 {
		queryRun = queryRun.Where("id = ?", runID)
	}
	result := queryRun.Order("created_at desc").Limit(1).Find(&run)
	if result.Error != nil {
		return nil, app_errors.ParseDBError(result.Error)
	}
	if result.RowsAffected == 0 {
		return &LeakScanEventsResponse{Events: []models.GroupLeakScanEvent{}, Pagination: map[string]any{"page": page, "page_size": pageSize, "total_items": 0, "total_pages": 0}}, nil
	}

	eventsQuery := s.db.WithContext(ctx).Model(&models.GroupLeakScanEvent{}).Where("group_id = ? AND run_id = ?", groupID, run.ID)
	var total int64
	if err := eventsQuery.Count(&total).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	var events []models.GroupLeakScanEvent
	if err := eventsQuery.Order("created_at desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&events).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	return &LeakScanEventsResponse{
		Run:    &run,
		Events: events,
		Pagination: map[string]any{
			"page": page, "page_size": pageSize, "total_items": total, "total_pages": int64(math.Ceil(float64(total) / float64(pageSize))),
		},
	}, nil
}

func (s *GroupLeakScanService) validateConfig(ctx context.Context, groupID uint, payload LeakScanConfigPayload) error {
	var group models.Group
	if err := s.db.WithContext(ctx).First(&group, groupID).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	if group.GroupType == "aggregate" && payload.Enabled {
		return NewI18nError(app_errors.ErrValidation, "validation.aggregate_no_leak_scan", nil)
	}
	if payload.AccountStrategy == "" {
		payload.AccountStrategy = models.LeakScanAccountStrategyRoundRobin
	}
	if payload.AccountStrategy != models.LeakScanAccountStrategyRoundRobin && payload.AccountStrategy != models.LeakScanAccountStrategyRandom {
		return NewI18nError(app_errors.ErrValidation, "validation.invalid_strategy", nil)
	}
	if !payload.Enabled {
		return nil
	}
	if len(payload.AccountIDs) == 0 || len(payload.SearchRules) == 0 || len(payload.MatchRules) == 0 {
		return NewI18nError(app_errors.ErrValidation, "validation.required_fields_missing", nil)
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.GitHubSearchAccount{}).
		Where("id IN ? AND status = ?", payload.AccountIDs, models.SearchAccountStatusActive).Count(&count).Error; err != nil {
		return app_errors.ParseDBError(err)
	}
	if count != int64(len(payload.AccountIDs)) {
		return NewI18nError(app_errors.ErrValidation, "validation.account_not_found", nil)
	}
	return nil
}

func (s *GroupLeakScanService) run(ctx context.Context, runID, groupID uint) {
	defer s.clearCancel(groupID)
	logger := logrus.WithFields(logrus.Fields{"run_id": runID, "group_id": groupID})
	if err := s.executeRun(ctx, runID, groupID); err != nil {
		logger.WithError(err).Error("leak scan failed")
		now := time.Now()
		status := models.LeakScanStatusFailed
		if ctx.Err() != nil {
			status = models.LeakScanStatusInterrupted
		}
		_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{"status": status, "error_message": err.Error(), "finished_at": &now}).Error
		_ = s.logEvent(context.Background(), runID, groupID, "run_failed", "error", err.Error(), nil)
	}
}

func (s *GroupLeakScanService) executeRun(ctx context.Context, runID, groupID uint) error {
	var group models.Group
	if err := s.db.First(&group, groupID).Error; err != nil {
		return err
	}
	if s.groupManager != nil {
		if cached, err := s.groupManager.GetGroupByName(group.Name); err == nil {
			group = *cached
		}
	}
	cfg, err := s.GetConfig(ctx, groupID)
	if err != nil {
		return err
	}
	payload, err := configToPayload(cfg)
	if err != nil {
		return err
	}
	if !payload.Enabled {
		return fmt.Errorf("leak scan is disabled")
	}
	_ = s.logEvent(ctx, runID, groupID, "run_started", "info", "泄露扫描任务开始", nil)

	for _, query := range payload.SearchRules {
		if err := ctx.Err(); err != nil {
			return err
		}
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("current_query", query).Error
		for _, sourceType := range normalizedSourceTypes(payload.SourceTypes) {
			if err := s.executeSearchSource(ctx, runID, groupID, &group, payload, query, sourceType); err != nil {
				return err
			}
		}
	}
	now := time.Now()
	if err := s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{"status": models.LeakScanStatusCompleted, "finished_at": &now}).Error; err != nil {
		return err
	}
	return s.logEvent(ctx, runID, groupID, "run_completed", "info", "泄露扫描任务完成", nil)
}

func (s *GroupLeakScanService) executeSearchSource(ctx context.Context, runID, groupID uint, group *models.Group, payload LeakScanConfigPayload, query, sourceType string) error {
	account, err := s.pickAccount(ctx, payload.AccountIDs, sourceType, payload.AccountStrategy)
	if err != nil {
		return err
	}
	accountLabel := fmt.Sprintf("#%d", account.ID)
	if account.Username != "" {
		accountLabel = "#" + account.Username
	}
	_ = s.logEvent(ctx, runID, groupID, "account_selected", "info", fmt.Sprintf("使用 %s 账户 %s", account.Type, accountLabel), map[string]any{"account_id": account.ID, "account_type": account.Type, "username": account.Username})

	maxPages := effectiveMaxPages(payload.MaxPages, sourceType)
	seenResults := map[string]bool{}
	seenQueries := map[string]bool{query: true}
	queries := []string{query}
	for queryIndex := 0; queryIndex < len(queries); queryIndex++ {
		currentQuery := queries[queryIndex]
		firstPage := true
		totalPages := 1
		_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("current_query", currentQuery).Error
		for page := 1; page <= totalPages && page <= maxPages; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			results, total, err := s.searchGitHub(ctx, account, currentQuery, sourceType, page)
			s.searchAccountService.MarkUsed(ctx, account.ID, err == nil)
			if err != nil {
				_ = s.incrementRun(runID, map[string]any{"failed_count": gorm.Expr("failed_count + ?", 1)})
				_ = s.logEvent(ctx, runID, groupID, "search_failed", "error", err.Error(), map[string]any{"query": currentQuery, "source_type": sourceType, "page": page})
				continue
			}
			if page == 1 && total > 0 {
				totalPages = int(math.Ceil(float64(total) / float64(githubSearchPerPage)))
				if totalPages < 1 {
					totalPages = 1
				}
			}
			if totalPages > maxPages {
				totalPages = maxPages
			}
			if page == 1 && payload.DeepIndex && total > int64(maxPages*githubSearchPerPage) {
				refined := generateRefinedQueries(currentQuery, githubMaxRefinedQueries)
				added := 0
				for _, refinedQuery := range refined {
					if !seenQueries[refinedQuery] {
						seenQueries[refinedQuery] = true
						queries = append(queries, refinedQuery)
						added++
					}
				}
				if added > 0 {
					_ = s.logEvent(ctx, runID, groupID, "deep_index_generated", "info", fmt.Sprintf("深度索引生成 %d 个细分查询", added), map[string]any{"query": currentQuery, "total": total, "limit": maxPages * githubSearchPerPage, "generated": added})
				}
			}
			updates := map[string]any{"processed_pages": gorm.Expr("processed_pages + ?", 1)}
			if page == 1 {
				updates["expected_search_items"] = gorm.Expr("expected_search_items + ?", total)
				updates["expected_pages"] = gorm.Expr("expected_pages + ?", totalPages)
			}
			_ = s.incrementRun(runID, updates)
			_ = s.logEvent(ctx, runID, groupID, "search_page_completed", "info", fmt.Sprintf("第 %d 页搜索完成，返回 %d 条结果", page, len(results)), map[string]any{"query": currentQuery, "source_type": sourceType, "page": page, "search_results": len(results), "total": total, "max_pages": maxPages})
			if firstPage {
				_ = s.logEvent(ctx, runID, groupID, "first_page_results", "info", "第一页搜索结果", map[string]any{"query": currentQuery, "source_type": sourceType, "items": results})
				firstPage = false
			}

			fetchedCount := 0
			var candidates []leakCandidate
			for _, result := range results {
				if result.URL == "" || seenResults[result.URL] {
					continue
				}
				seenResults[result.URL] = true
				body, err := s.fetchGitHubContent(ctx, account, result.URL)
				if err != nil || body == "" {
					continue
				}
				fetchedCount++
				candidates = append(candidates, extractCandidatesFromFile(body, payload.MatchRules, result.URL)...)
			}
			_ = s.logEvent(ctx, runID, groupID, "content_fetched", "info", fmt.Sprintf("第 %d 页获取文件内容 %d/%d 个", page, fetchedCount, len(results)), map[string]any{"query": currentQuery, "page": page, "fetched": fetchedCount, "results": len(results)})
			_ = s.incrementRun(runID, map[string]any{"collected_count": gorm.Expr("collected_count + ?", len(candidates))})
			_ = s.logEvent(ctx, runID, groupID, "keys_extracted", "info", fmt.Sprintf("第 %d 页提取候选 key %d 个", page, len(candidates)), map[string]any{"query": currentQuery, "page": page, "extracted": len(candidates), "items": redactCandidatePayload(candidates, 30)})
			for _, candidate := range candidates {
				if err := s.processCandidate(ctx, runID, group, candidate, sourceType, currentQuery); err != nil {
					_ = s.logEvent(ctx, runID, groupID, "candidate_failed", "error", err.Error(), map[string]any{"query": currentQuery, "url": candidate.URL, "line": candidate.Line})
				}
			}
			if len(results) == 0 {
				break
			}
		}
	}
	return nil
}

func (s *GroupLeakScanService) processCandidate(ctx context.Context, runID uint, group *models.Group, candidate leakCandidate, sourceType, query string) error {
	keyValue := candidate.Value
	keyHash := s.keyService.EncryptionSvc.Hash(keyValue)
	if keyHash == "" {
		return nil
	}
	var existing models.APIKey
	err := s.db.WithContext(ctx).Where("group_id = ? AND key_hash = ?", group.ID, keyHash).First(&existing).Error
	if err == nil {
		_ = s.incrementRun(runID, map[string]any{"duplicate_count": gorm.Expr("duplicate_count + ?", 1)})
		return s.logEvent(ctx, runID, group.ID, "deduplicated", "info", "候选 key 已存在于当前分组，跳过", map[string]any{"status": existing.Status})
	}
	if err != gorm.ErrRecordNotFound {
		return err
	}
	encrypted, err := s.keyService.EncryptionSvc.Encrypt(keyValue)
	if err != nil {
		return err
	}
	apiKey := models.APIKey{GroupID: group.ID, KeyValue: encrypted, KeyHash: keyHash, Status: models.KeyStatusRecorded}
	if err := s.keyService.KeyProvider.AddKeys(group.ID, []models.APIKey{apiKey}); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Where("group_id = ? AND key_hash = ?", group.ID, keyHash).First(&apiKey).Error; err != nil {
		return err
	}
	_ = s.logEvent(ctx, runID, group.ID, "recorded_created", "info", "候选 key 已写入记录状态", map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
	valid, validationErr := s.keyService.KeyValidator.ValidateCandidateKey(keyValue, group)
	if valid {
		if err := s.keyService.KeyProvider.UpdateKeyStatus(group.ID, apiKey.ID, models.KeyStatusActive); err != nil {
			return err
		}
		_ = s.incrementRun(runID, map[string]any{"valid_count": gorm.Expr("valid_count + ?", 1), "imported_count": gorm.Expr("imported_count + ?", 1)})
		return s.logEvent(ctx, runID, group.ID, "key_marked_active", "info", "模型检测通过，key 已标记为有效", map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
	}
	if err := s.keyService.KeyProvider.UpdateKeyStatus(group.ID, apiKey.ID, models.KeyStatusInvalid); err != nil {
		return err
	}
	_ = s.incrementRun(runID, map[string]any{"invalid_count": gorm.Expr("invalid_count + ?", 1)})
	msg := "模型检测不通过，key 已标记为无效"
	if validationErr != nil {
		msg = validationErr.Error()
	}
	return s.logEvent(ctx, runID, group.ID, "key_marked_invalid", "info", msg, map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
}

func (s *GroupLeakScanService) pickAccount(ctx context.Context, ids []uint, sourceType, strategy string) (*models.GitHubSearchAccount, error) {
	var accounts []models.GitHubSearchAccount
	if err := s.db.WithContext(ctx).Where("id IN ? AND type = ? AND status = ?", ids, sourceType, models.SearchAccountStatusActive).Find(&accounts).Error; err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no active %s search account", sourceType)
	}
	if strategy == models.LeakScanAccountStrategyRandom {
		return &accounts[rand.Intn(len(accounts))], nil
	}
	return &accounts[0], nil
}

func (s *GroupLeakScanService) searchGitHub(ctx context.Context, account *models.GitHubSearchAccount, query, sourceType string, page int) ([]leakSearchResult, int64, error) {
	if sourceType == models.SearchAccountTypeGitHubAPI {
		return s.searchGitHubAPI(ctx, account, query, page)
	}
	return s.searchGitHubWeb(ctx, account, query, page)
}

func (s *GroupLeakScanService) searchGitHubAPI(ctx context.Context, account *models.GitHubSearchAccount, query string, page int) ([]leakSearchResult, int64, error) {
	endpoint := "https://api.github.com/search/code?q=" + url.QueryEscape(query) + fmt.Sprintf("&sort=indexed&order=desc&per_page=%d&page=%d", githubSearchPerPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+account.Credential)
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	resp, err := s.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("github api search failed: status %d", resp.StatusCode)
	}
	var payload struct {
		TotalCount int64 `json:"total_count"`
		Items      []struct {
			HTMLURL string `json:"html_url"`
			URL     string `json:"url"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, 0, err
	}
	urls := make([]leakSearchResult, 0, len(payload.Items))
	for _, item := range payload.Items {
		if item.HTMLURL != "" {
			urls = append(urls, leakSearchResult{URL: item.HTMLURL})
		} else if item.URL != "" {
			urls = append(urls, leakSearchResult{URL: item.URL})
		}
	}
	return urls, payload.TotalCount, nil
}

func (s *GroupLeakScanService) searchGitHubWeb(ctx context.Context, account *models.GitHubSearchAccount, query string, page int) ([]leakSearchResult, int64, error) {
	endpoint := "https://github.com/search?type=code&o=desc&p=" + strconv.Itoa(page) + "&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	req.Header.Set("Cookie", "user_session="+account.Credential)
	resp, err := s.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("github web search failed: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	re := regexp.MustCompile(`href="(/[^\s"]+/blob/(?:[^"]+)?)#L\d+"`)
	matches := re.FindAllStringSubmatch(string(body), -1)
	seen := map[string]bool{}
	var urls []leakSearchResult
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		u := "https://github.com" + match[1]
		if !seen[u] {
			seen[u] = true
			urls = append(urls, leakSearchResult{URL: u, Line: extractGitHubLine(u)})
		}
	}
	total := int64(len(urls))
	if page == 1 {
		total = s.estimateGitHubWebTotal(ctx, account, query, string(body), total)
	}
	return urls, total, nil
}

func (s *GroupLeakScanService) fetchGitHubContent(ctx context.Context, account *models.GitHubSearchAccount, resultURL string) (string, error) {
	if strings.Contains(resultURL, "api.github.com/repos/") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+account.Credential)
		req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
		resp, err := s.httpClient(30 * time.Second).Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var payload struct{ Content, Encoding string }
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return "", err
		}
		if payload.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
			return string(decoded), err
		}
		return payload.Content, nil
	}
	rawURL := toRawGitHubURL(resultURL)
	if rawURL == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	resp, err := s.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func (s *GroupLeakScanService) estimateGitHubWebTotal(ctx context.Context, account *models.GitHubSearchAccount, query, pageContent string, fallback int64) int64 {
	encoded := url.QueryEscape(query)
	endpoint := "https://github.com/search/blackbird_count?saved_searches=^&q=" + encoded
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return extractGitHubWebCount(pageContent, fallback)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://github.com/search?q="+encoded+"^&type=code")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	req.Header.Set("Cookie", "user_session="+account.Credential)
	resp, err := s.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return extractGitHubWebCount(pageContent, fallback)
	}
	defer resp.Body.Close()
	var payload struct {
		Failed bool  `json:"failed"`
		Count  int64 `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && !payload.Failed && payload.Count > 0 {
		return payload.Count
	}
	return extractGitHubWebCount(pageContent, fallback)
}

func extractGitHubWebCount(content string, fallback int64) int64 {
	patterns := []string{`We\\'ve found ([\d,]+) code results`, `([\d,]+) code results`, `data-total-count="([\d,]+)"`, `"total_count":(\d+)`}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		match := re.FindStringSubmatch(content)
		if len(match) > 1 {
			value := strings.ReplaceAll(match[1], ",", "")
			if count, err := strconv.ParseInt(value, 10, 64); err == nil && count > 0 {
				return count
			}
		}
	}
	return fallback
}

func extractGitHubLine(resultURL string) int {
	u, err := url.Parse(resultURL)
	if err != nil {
		return 0
	}
	fragment := strings.TrimPrefix(u.Fragment, "L")
	line, _ := strconv.Atoi(fragment)
	return line
}

func toRawGitHubURL(input string) string {
	u, err := url.Parse(input)
	if err != nil || !strings.Contains(u.Host, "github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "blob" {
		return ""
	}
	return "https://raw.githubusercontent.com/" + parts[0] + "/" + parts[1] + "/" + strings.Join(parts[3:], "/")
}

func pickSearchAccountUserAgent(value string) string {
	type weightedUserAgent struct {
		value  string
		weight int
	}
	var userAgents []weightedUserAgent
	totalWeight := 0
	for _, item := range strings.Split(value, "\n") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		weight := 1
		if tabIndex := strings.Index(item, "\t"); tabIndex > 0 {
			if parsed, err := strconv.Atoi(strings.TrimSpace(item[:tabIndex])); err == nil && parsed > 0 {
				weight = parsed
				item = strings.TrimSpace(item[tabIndex+1:])
			}
		}
		if item != "" {
			userAgents = append(userAgents, weightedUserAgent{value: item, weight: weight})
			totalWeight += weight
		}
	}
	if len(userAgents) == 0 {
		return "Mozilla/5.0"
	}
	target := rand.Intn(totalWeight) + 1
	for _, item := range userAgents {
		target -= item.weight
		if target <= 0 {
			return item.value
		}
	}
	return userAgents[0].value
}

func extractCandidatesFromFile(content string, patterns []string, sourceURL string) []leakCandidate {
	seen := map[string]bool{}
	var result []leakCandidate
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err == nil {
			compiled = append(compiled, re)
		}
	}
	for lineIndex, line := range strings.Split(content, "\n") {
		for _, re := range compiled {
			for _, value := range re.FindAllString(line, -1) {
				value = strings.TrimSpace(value)
				if value == "" || seen[value] {
					continue
				}
				seen[value] = true
				result = append(result, leakCandidate{Value: value, URL: sourceURL, Line: lineIndex + 1})
			}
		}
	}
	return result
}

func redactCandidatePayload(candidates []leakCandidate, limit int) []map[string]any {
	if limit <= 0 || limit > len(candidates) {
		limit = len(candidates)
	}
	items := make([]map[string]any, 0, limit)
	for _, candidate := range candidates[:limit] {
		items = append(items, map[string]any{"key": maskSensitive(candidate.Value), "url": candidate.URL, "line": candidate.Line})
	}
	return items
}

func maskSensitive(value string) string {
	if len(value) <= 12 {
		return strings.Repeat("*", len(value))
	}
	return value[:6] + "..." + value[len(value)-6:]
}

func configToPayload(cfg *models.GroupLeakScanConfig) (LeakScanConfigPayload, error) {
	payload := LeakScanConfigPayload{Enabled: cfg.Enabled, AccountStrategy: cfg.AccountStrategy, MaxPages: cfg.MaxPages, DeepIndex: cfg.DeepIndex}
	_ = json.Unmarshal(cfg.SourceTypes, &payload.SourceTypes)
	_ = json.Unmarshal(cfg.AccountIDs, &payload.AccountIDs)
	_ = json.Unmarshal(cfg.SearchRules, &payload.SearchRules)
	_ = json.Unmarshal(cfg.MatchRules, &payload.MatchRules)
	if payload.AccountStrategy == "" {
		payload.AccountStrategy = models.LeakScanAccountStrategyRoundRobin
	}
	return payload, nil
}

func effectiveMaxPages(configured int, sourceType string) int {
	if configured > 0 {
		return configured
	}
	if sourceType == models.SearchAccountTypeGitHubAPI {
		return githubAPIDefaultMaxPages
	}
	return githubWebDefaultMaxPages
}

func generateRefinedQueries(query string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	re := regexp.MustCompile(`\[([^\]]+)\]\{(\d+)\}`)
	loc := re.FindStringSubmatchIndex(query)
	if len(loc) < 6 {
		return nil
	}
	charsetExpr := query[loc[2]:loc[3]]
	countValue := query[loc[4]:loc[5]]
	count, err := strconv.Atoi(countValue)
	if err != nil || count <= 1 {
		return nil
	}
	chars := expandRegexCharset(charsetExpr)
	if len(chars) == 0 {
		return nil
	}
	if len(chars) > limit {
		chars = chars[:limit]
	}
	prefix := query[:loc[0]]
	suffix := query[loc[1]:]
	rest := fmt.Sprintf("[%s]{%d}", charsetExpr, count-1)
	queries := make([]string, 0, len(chars))
	for _, char := range chars {
		queries = append(queries, prefix+escapeRegexLiteralChar(char)+rest+suffix)
	}
	return queries
}

func expandRegexCharset(expr string) []rune {
	seen := map[rune]bool{}
	var chars []rune
	runes := []rune(expr)
	for i := 0; i < len(runes); i++ {
		if i+2 < len(runes) && runes[i+1] == '-' && runes[i] <= runes[i+2] {
			for char := runes[i]; char <= runes[i+2]; char++ {
				if !seen[char] {
					seen[char] = true
					chars = append(chars, char)
				}
			}
			i += 2
			continue
		}
		if !seen[runes[i]] {
			seen[runes[i]] = true
			chars = append(chars, runes[i])
		}
	}
	return chars
}

func escapeRegexLiteralChar(char rune) string {
	if strings.ContainsRune(`\.+*?()|{}[]^$-`, char) {
		return `\` + string(char)
	}
	return string(char)
}

func LeakScanConfigFromModel(cfg *models.GroupLeakScanConfig) (LeakScanConfigPayload, error) {
	return configToPayload(cfg)
}

func normalizedSourceTypes(types []string) []string {
	if len(types) == 0 {
		return []string{models.SearchAccountTypeGitHubAPI}
	}
	return types
}

func (s *GroupLeakScanService) incrementRun(runID uint, updates map[string]any) error {
	return s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(updates).Error
}

func (s *GroupLeakScanService) logEvent(ctx context.Context, runID, groupID uint, eventType, level, message string, payload map[string]any) error {
	var raw datatypes.JSON
	if payload != nil {
		bytes, _ := json.Marshal(payload)
		raw = datatypes.JSON(bytes)
	}
	return s.db.WithContext(ctx).Create(&models.GroupLeakScanEvent{RunID: runID, GroupID: groupID, EventType: eventType, Level: level, Message: message, Payload: raw, CreatedAt: time.Now()}).Error
}

func (s *GroupLeakScanService) clearCancel(groupID uint) {
	s.mu.Lock()
	delete(s.cancelByGroup, groupID)
	s.mu.Unlock()
}
