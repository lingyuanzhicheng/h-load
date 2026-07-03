package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"h-load/internal/config"
	app_errors "h-load/internal/errors"
	"h-load/internal/httpclient"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"h-load/internal/models"

	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const githubAPISearchPerPage = 100
const githubWebSearchPerPage = 20
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
	stopRequested        map[uint]bool // 用户请求暂停的标志，任务在当前页密钥验证完成后检查并暂停
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
		stopRequested:        make(map[uint]bool),
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
	// Start 用于停止(failed)后重新启动，不清空历史记录，从头重新开始
	return s.startWithCleanup(ctx, groupID, false)
}

func (s *GroupLeakScanService) Resume(ctx context.Context, groupID uint) (*models.GroupLeakScanRun, error) {
	// Resume 用于暂停(interrupted)后恢复，从断点继续
	return s.startWithCleanup(ctx, groupID, false, true)
}

func (s *GroupLeakScanService) startWithCleanup(ctx context.Context, groupID uint, clearOld bool, isResume ...bool) (*models.GroupLeakScanRun, error) {
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
	s.stopRequested[groupID] = false
	s.mu.Unlock()

	resumeMode := len(isResume) > 0 && isResume[0]
	var run models.GroupLeakScanRun

	if resumeMode {
		// 恢复模式：查找最近一条 interrupted 的 run，继续使用
		if err := s.db.WithContext(ctx).Where("group_id = ? AND status = ?", groupID, models.LeakScanStatusInterrupted).
			Order("id DESC").First(&run).Error; err != nil {
			// 没有 interrupted 的 run，退回全新开始
			resumeMode = false
		}
	}

	if clearOld {
		s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanEvent{})
		s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanRun{})
	}

	if !resumeMode {
		// 全新开始：创建新 run
		now := time.Now()
		run = models.GroupLeakScanRun{GroupID: groupID, Status: models.LeakScanStatusRunning, StartedAt: &now}
		if err := s.db.WithContext(ctx).Create(&run).Error; err != nil {
			s.clearCancel(groupID)
			return nil, app_errors.ParseDBError(err)
		}
		logrus.WithFields(logrus.Fields{"run_id": run.ID, "group_id": groupID}).Info("leak scan run created")
		_ = s.logEvent(ctx, run.ID, groupID, "run_created", "info", "泄露扫描任务已创建", nil)
	} else {
		// 恢复模式：更新现有 run 状态为 running
		now := time.Now()
		s.db.WithContext(ctx).Model(&run).Updates(map[string]any{
			"status":      models.LeakScanStatusRunning,
			"finished_at": nil,
			"started_at":  &now,
		})
		_ = s.logEvent(ctx, run.ID, groupID, "run_resumed", "info", "泄露扫描任务从断点恢复", nil)
	}

	go s.run(runCtx, run.ID, groupID, resumeMode)
	return &run, nil
}

// Stop 请求暂停任务。不是立即取消，而是设置 stopRequested 标志，
// 任务在完成当前页的密钥验证后优雅暂停。
func (s *GroupLeakScanService) Stop(ctx context.Context, groupID uint) error {
	s.mu.Lock()
	_, exists := s.cancelByGroup[groupID]
	if exists {
		// 设置暂停标志，任务自行检测并暂停
		s.stopRequested[groupID] = true
		// 立即更新状态为 stopping（暂停中），前端显示动态黄圈
		s.db.Model(&models.GroupLeakScanRun{}).
			Where("group_id = ? AND (status = ? OR status = ?)", groupID, models.LeakScanStatusRunning, models.LeakScanStatusWaiting).
			Update("status", models.LeakScanStatusStopping)
	}
	s.mu.Unlock()
	if !exists {
		return nil
	}

	// 启动一个 goroutine 等待任务实际暂停（最多等待 5 分钟）
	go func() {
		timeout := time.After(5 * time.Minute)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-timeout:
				// 超时强制取消
				s.mu.Lock()
				if cancel, ok := s.cancelByGroup[groupID]; ok {
					cancel()
					delete(s.cancelByGroup, groupID)
					delete(s.stopRequested, groupID)
				}
				s.mu.Unlock()
				now := time.Now()
				s.db.Model(&models.GroupLeakScanRun{}).
					Where("group_id = ? AND status = ?", groupID, models.LeakScanStatusStopping).
					Updates(map[string]any{"status": models.LeakScanStatusInterrupted, "finished_at": &now})
				return
			case <-ticker.C:
				// 检查任务是否已暂停（cancelByGroup 中已删除说明 run 函数已退出）
				s.mu.Lock()
				_, stillRunning := s.cancelByGroup[groupID]
				s.mu.Unlock()
				if !stillRunning {
					return
				}
			}
		}
	}()

	return nil
}

// isStopRequested 检查是否请求了暂停
func (s *GroupLeakScanService) isStopRequested(groupID uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopRequested[groupID]
}

func (s *GroupLeakScanService) Reset(ctx context.Context, groupID uint) (*models.GroupLeakScanRun, error) {
	_ = s.Stop(ctx, groupID)
	return s.Start(ctx, groupID)
}

// Initialize 初始化泄露扫描任务：强制停止运行中的任务，清空所有记录，重置为初始状态。
// 与 Stop 不同，这里是立即取消不等收尾，且清空历史记录。
func (s *GroupLeakScanService) Initialize(ctx context.Context, groupID uint) error {
	s.mu.Lock()
	if cancel, exists := s.cancelByGroup[groupID]; exists {
		cancel()
		delete(s.cancelByGroup, groupID)
		delete(s.stopRequested, groupID)
	}
	s.mu.Unlock()
	s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanEvent{})
	s.db.WithContext(ctx).Where("group_id = ?", groupID).Delete(&models.GroupLeakScanRun{})
	return nil
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
	if payload.AccountStrategy == "" || payload.AccountStrategy == "random" {
		payload.AccountStrategy = models.LeakScanAccountStrategyRoundRobin
	}
	if payload.AccountStrategy != models.LeakScanAccountStrategyRoundRobin && payload.AccountStrategy != models.LeakScanAccountStrategyBalanced {
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

func (s *GroupLeakScanService) run(ctx context.Context, runID, groupID uint, resumeMode bool) {
	defer s.clearCancel(groupID)
	logger := logrus.WithFields(logrus.Fields{"run_id": runID, "group_id": groupID})
	if err := s.executeRun(ctx, runID, groupID, resumeMode); err != nil {
		// 用户请求暂停：executeSearchSource 已设置 interrupted 状态，不需要再覆盖
		if s.isStopRequested(groupID) {
			s.mu.Lock()
			delete(s.stopRequested, groupID)
			s.mu.Unlock()
			return
		}
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

func (s *GroupLeakScanService) executeRun(ctx context.Context, runID, groupID uint, resumeMode bool) error {
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

	// 读取断点信息
	var resumeQueryIdx, resumeSourceIdx, resumePage int
	var resumeLastAccountID uint
	if resumeMode {
		var run models.GroupLeakScanRun
		if err := s.db.First(&run, runID).Error; err == nil {
			resumeQueryIdx = run.ResumeQueryIndex
			resumeSourceIdx = run.ResumeSourceTypeIdx
			resumePage = run.ResumePage
			resumeLastAccountID = run.ResumeLastAccountID
		}
		_ = s.logEvent(ctx, runID, groupID, "run_started", "info", fmt.Sprintf("泄露扫描任务从断点恢复（query=%d, source=%d, page=%d）", resumeQueryIdx, resumeSourceIdx, resumePage), nil)
	} else {
		_ = s.logEvent(ctx, runID, groupID, "run_started", "info", "泄露扫描任务开始", nil)
	}

	// 为每个 sourceType 创建账户轮换器
	sourceTypes := normalizedSourceTypes(payload.SourceTypes)
	rotators := make(map[string]*accountRotator)
	for _, sourceType := range sourceTypes {
		rotators[sourceType] = &accountRotator{strategy: payload.AccountStrategy, lastUsedID: resumeLastAccountID}
	}

	for queryIdx, query := range payload.SearchRules {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 断点续扫：跳过已完成的 query
		if resumeMode && queryIdx < resumeQueryIdx {
			continue
		}
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("current_query", query).Error
		for sourceIdx, sourceType := range sourceTypes {
			// 断点续扫：跳过已完成的 sourceType
			if resumeMode && queryIdx == resumeQueryIdx && sourceIdx < resumeSourceIdx {
				continue
			}

			// 记录断点进度
			_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{
				"resume_query_index":     queryIdx,
				"resume_source_type_idx": sourceIdx,
				"resume_page":            0,
			}).Error

			if err := s.executeSearchSource(ctx, runID, groupID, &group, payload, query, sourceType, rotators[sourceType], resumeMode, resumePage); err != nil {
				return err
			}
			// resumeMode 只对第一个 sourceType 生效
			resumePage = 0
		}
	}
	now := time.Now()
	if err := s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{"status": models.LeakScanStatusCompleted, "finished_at": &now}).Error; err != nil {
		return err
	}
	return s.logEvent(ctx, runID, groupID, "run_completed", "info", "泄露扫描任务完成", nil)
}

func (s *GroupLeakScanService) executeSearchSource(ctx context.Context, runID, groupID uint, group *models.Group, payload LeakScanConfigPayload, query, sourceType string, rotator *accountRotator, resumeMode bool, resumePage int) error {
	account, err := s.pickAccountWithWait(ctx, runID, groupID, rotator, payload.AccountIDs, sourceType)
	if err != nil {
		return err
	}
	accountLabel := fmt.Sprintf("#%d", account.ID)
	if account.Username != "" {
		accountLabel = "#" + account.Username
	}
	_ = s.logEvent(ctx, runID, groupID, "account_selected", "info", fmt.Sprintf("使用 %s 账户 %s", account.Type, accountLabel), map[string]any{"account_id": account.ID, "account_type": account.Type, "username": account.Username})

	maxPages := effectiveMaxPages(payload.MaxPages, sourceType)
	perPage := effectivePerPage(sourceType)
	seenResults := map[string]bool{}
	// API 模式下将正则查询转换为关键词
	processedQuery := query
	if sourceType == models.SearchAccountTypeGitHubAPI {
		processedQuery = preprocessQueryForAPI(query)
	}
	seenQueries := map[string]bool{processedQuery: true}
	// queries 存储预处理后的查询（用于搜索），originalQueries 存储原始查询（用于深度索引）
	queries := []string{processedQuery}
	originalQueries := []string{query}

	// 断点续扫：确定起始页
	startPage := 1
	if resumeMode && resumePage > 0 {
		startPage = resumePage
	}

	for queryIndex := 0; queryIndex < len(queries); queryIndex++ {
		currentQuery := queries[queryIndex]
		originalQuery := originalQueries[queryIndex]
		firstPage := true
		totalPages := 1
		_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("current_query", currentQuery).Error
		for page := startPage; page <= totalPages && page <= maxPages; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}

			// 记录断点：当前页码
			_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("resume_page", page).Error

			results, total, err := s.searchGitHub(ctx, account, currentQuery, sourceType, page)
			isRateLimited := err != nil && strings.Contains(err.Error(), "status 429")
			if isRateLimited {
				s.searchAccountService.MarkUsedWithRateLimit(ctx, account.ID, false, true)
				s.searchAccountService.MarkRateLimited(ctx, account.ID)
				_ = s.logEvent(ctx, runID, groupID, "account_rate_limited", "warning", fmt.Sprintf("账户被限流，跳过当前页: %s", err.Error()), map[string]any{"query": currentQuery, "source_type": sourceType, "page": page})
				// 尝试获取其他可用账户，若全部受限则进入等待
				newAccount, waitErr := s.pickAccountWithWait(ctx, runID, groupID, rotator, payload.AccountIDs, sourceType)
				if waitErr != nil {
					return waitErr
				}
				account = newAccount
				continue
			}
			s.searchAccountService.MarkUsed(ctx, account.ID, err == nil)
			if err != nil {
				_ = s.incrementRun(runID, map[string]any{"failed_count": gorm.Expr("failed_count + ?", 1)})
				_ = s.logEvent(ctx, runID, groupID, "search_failed", "error", err.Error(), map[string]any{"query": currentQuery, "source_type": sourceType, "page": page})
				// 非 429 失败也切换账户
				newAccount, waitErr := s.pickAccountWithWait(ctx, runID, groupID, rotator, payload.AccountIDs, sourceType)
				if waitErr != nil {
					return waitErr
				}
				account = newAccount
				continue
			}
			if page == 1 && total > 0 {
				totalPages = int(math.Ceil(float64(total) / float64(perPage)))
				if totalPages < 1 {
					totalPages = 1
				}
			}
			if totalPages > maxPages {
				totalPages = maxPages
			}
			if page == 1 && payload.DeepIndex && total > int64(maxPages*perPage) {
				// 用原始查询（非预处理）生成细分查询，因为 generateRefinedQueries 需要正则格式
				refined := generateRefinedQueries(originalQuery, githubMaxRefinedQueries)
				added := 0
				for _, refinedQuery := range refined {
					// API 模式下对细分查询做预处理
					processedRefined := refinedQuery
					if sourceType == models.SearchAccountTypeGitHubAPI {
						processedRefined = preprocessQueryForAPI(refinedQuery)
					}
					if !seenQueries[processedRefined] {
						seenQueries[processedRefined] = true
						queries = append(queries, processedRefined)
						originalQueries = append(originalQueries, refinedQuery)
						added++
					}
				}
				if added > 0 {
					_ = s.logEvent(ctx, runID, groupID, "deep_index_generated", "info", fmt.Sprintf("深度索引生成 %d 个细分查询", added), map[string]any{"query": currentQuery, "total": total, "limit": maxPages * perPage, "generated": added})
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

			// 密钥验证：必须完成所有候选 key 的验证后才检查暂停
			for _, candidate := range candidates {
				if err := s.processCandidate(ctx, runID, group, candidate, sourceType, currentQuery); err != nil {
					_ = s.logEvent(ctx, runID, groupID, "candidate_failed", "error", err.Error(), map[string]any{"query": currentQuery, "url": candidate.URL, "line": candidate.Line})
				}
			}

			// 优雅暂停检查：当前页密钥验证全部完成后，检查是否请求了暂停
			if s.isStopRequested(groupID) {
				// 记录断点信息：当前页已完成，恢复时从下一页开始
				_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{
					"resume_page":            page + 1,
					"resume_last_account_id": rotator.lastUsedID,
				}).Error
				_ = s.logEvent(ctx, runID, groupID, "run_stopped", "info", fmt.Sprintf("任务暂停，当前页 %d 密钥验证已完成，恢复时从第 %d 页继续", page, page+1), nil)
				now := time.Now()
				_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Updates(map[string]any{"status": models.LeakScanStatusInterrupted, "finished_at": &now}).Error
				return fmt.Errorf("task stopped by user request")
			}

			if len(results) == 0 {
				break
			}
		}
		// 后续查询从第 1 页开始
		startPage = 1
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
		return s.logEvent(ctx, runID, group.ID, "key_marked_active", "info", "验证有效，key 已标记为有效", map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
	}
	if errors.Is(validationErr, app_errors.ErrKeyRateLimited) {
		if err := s.keyService.KeyProvider.UpdateKeyStatus(group.ID, apiKey.ID, models.KeyStatusLimited); err != nil {
			return err
		}
		_ = s.incrementRun(runID, map[string]any{"limited_count": gorm.Expr("limited_count + ?", 1), "imported_count": gorm.Expr("imported_count + ?", 1)})
		return s.logEvent(ctx, runID, group.ID, "key_marked_limited", "info", "验证429，key 已标记为受限", map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
	}
	if err := s.keyService.KeyProvider.UpdateKeyStatus(group.ID, apiKey.ID, models.KeyStatusInvalid); err != nil {
		return err
	}
	_ = s.incrementRun(runID, map[string]any{"invalid_count": gorm.Expr("invalid_count + ?", 1)})
	msg := "验证无效，key 已标记为无效"
	if validationErr != nil {
		msg = validationErr.Error()
	}
	return s.logEvent(ctx, runID, group.ID, "key_marked_invalid", "info", msg, map[string]any{"key_id": apiKey.ID, "url": candidate.URL, "line": candidate.Line})
}

// accountRotator 管理账户轮换状态，记录上次使用的账户 ID 和策略。
// 每次选账户时实时从 DB 同步最新状态，在排序后的列表中从 lastUsedID 的下一个开始找第一个 active。
type accountRotator struct {
	strategy   string
	lastUsedID uint // 上次使用的账户 ID，0 表示尚未使用过
}

// syncAndCheckAvailability 同步 DB 状态并检查账户可用性。
// 流程：查询全部 → 剔除 inactive → 检查全无效 → 剔除 limited → 检查全受限 → 返回 active 排序列表。
// 返回：
//   - accountAvailable + active 排序列表：有可用账户
//   - accountAllLimited + nil：全部受限，需等待
//   - accountAllInvalid + nil：全部无效，需停止
func (s *GroupLeakScanService) syncAndCheckAvailability(ctx context.Context, ids []uint, sourceType, strategy string) (accountAvailability, []models.GitHubSearchAccount, error) {
	// 1. 查询所有配置账户的最新状态
	var allAccounts []models.GitHubSearchAccount
	if err := s.db.WithContext(ctx).
		Where("id IN ? AND type = ?", ids, sourceType).
		Find(&allAccounts).Error; err != nil {
		return accountAllInvalid, nil, err
	}

	// 2. 剔除 inactive，保留 active 和 limited（先统计 limited 用于全受限检查）
	var activeAccounts []models.GitHubSearchAccount
	var limitedCount int
	for i := range allAccounts {
		switch allAccounts[i].Status {
		case models.SearchAccountStatusActive:
			activeAccounts = append(activeAccounts, allAccounts[i])
		case models.SearchAccountStatusLimited:
			limitedCount++
		}
		// inactive 被剔除
	}

	// 3. 全 inactive（无 active 无 limited）→ 终止
	if len(activeAccounts) == 0 && limitedCount == 0 {
		return accountAllInvalid, nil, nil
	}

	// 4. 无 active 但有 limited → 全受限，等待
	if len(activeAccounts) == 0 && limitedCount > 0 {
		return accountAllLimited, nil, nil
	}

	// 5. 有 active 账户 → 排序后返回（只含 active，limited 已剔除）
	if strategy == models.LeakScanAccountStrategyBalanced {
		sort.SliceStable(activeAccounts, func(i, j int) bool {
			if activeAccounts[i].RequestCount != activeAccounts[j].RequestCount {
				return activeAccounts[i].RequestCount < activeAccounts[j].RequestCount
			}
			return activeAccounts[i].ID < activeAccounts[j].ID
		})
	} else {
		sort.SliceStable(activeAccounts, func(i, j int) bool {
			return activeAccounts[i].ID < activeAccounts[j].ID
		})
	}

	return accountAvailable, activeAccounts, nil
}

// accountAvailability 表示任务关联账户的可用性状态。
type accountAvailability int

const (
	accountAvailable     accountAvailability = iota // 有可用账户
	accountAllLimited                               // 全部受限，需等待
	accountAllInvalid                               // 全部无效，需停止
)

// pickAccountWithWait 实时同步 DB 状态并选择下一个可用账户。
// 每次调用都执行完整流程：同步 → 剔除 inactive → 检查全无效 → 剔除 limited → 检查全受限 → 等待/选账户。
// 在排序后的 active 列表中，找比 lastUsedID 大的第一个 ID，没有则绕回头部。
func (s *GroupLeakScanService) pickAccountWithWait(ctx context.Context, runID, groupID uint, rotator *accountRotator, ids []uint, sourceType string) (*models.GitHubSearchAccount, error) {
	if rotator == nil {
		return nil, fmt.Errorf("no search account rotator")
	}

	// 循环：同步 → 检查可用性 → 等待/选择
	for {
		availability, sortedAccounts, err := s.syncAndCheckAvailability(ctx, ids, sourceType, rotator.strategy)
		if err != nil {
			return nil, err
		}

		switch availability {
		case accountAllInvalid:
			// 全部无效，终止任务
			return nil, fmt.Errorf("所有账户已处于无效状态，任务停止")

		case accountAllLimited:
			// 全部受限，进入等待状态
			_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("status", models.LeakScanStatusWaiting).Error
			_ = s.logEvent(ctx, runID, groupID, "account_waiting", "warning", "所有账户处于受限状态，任务等待中", map[string]any{"source_type": sourceType})

			// 30 秒轮询等待账户恢复
			ticker := time.NewTicker(30 * time.Second)
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					return nil, ctx.Err()
				case <-ticker.C:
					availability, _, _ = s.syncAndCheckAvailability(ctx, ids, sourceType, rotator.strategy)
					if availability == accountAvailable {
						ticker.Stop()
						_ = s.db.Model(&models.GroupLeakScanRun{}).Where("id = ?", runID).Update("status", models.LeakScanStatusRunning).Error
						_ = s.logEvent(ctx, runID, groupID, "account_recovered", "info", "账户已恢复可用，任务继续", map[string]any{"source_type": sourceType})
						// 重新进入外层循环，重新同步并选账户
						goto retry
					}
					if availability == accountAllInvalid {
						ticker.Stop()
						return nil, fmt.Errorf("所有账户已处于无效状态，任务停止")
					}
				}
			}

		case accountAvailable:
			// 有可用账户，按策略选择
			if len(sortedAccounts) == 0 {
				return nil, fmt.Errorf("no active %s search account found after sync", sourceType)
			}

			var selected *models.GitHubSearchAccount
			if rotator.strategy == models.LeakScanAccountStrategyBalanced {
				// 均衡：始终取 request_count 最小的（排序后第一个）
				selected = &sortedAccounts[0]
			} else {
				// 轮询：找比 lastUsedID 大的第一个 ID，没有则绕回头部
				selected = selectNextAccount(sortedAccounts, rotator.lastUsedID)
			}

			if selected == nil {
				return nil, fmt.Errorf("no active %s search account found after sync", sourceType)
			}
			rotator.lastUsedID = selected.ID
			return selected, nil
		}

	retry:
	}
}

// selectNextAccount 在排序后的 active 账户列表中，找比 lastUsedID 大的第一个账户。
// 如果 lastUsedID 为 0（尚未使用过），或比所有 ID 都大，则从列表第一个开始（绕回头部）。
// 列表中全是 active 账户，无需跳过 limited。
func selectNextAccount(sortedAccounts []models.GitHubSearchAccount, lastUsedID uint) *models.GitHubSearchAccount {
	if len(sortedAccounts) == 0 {
		return nil
	}

	// lastUsedID=0（首次），直接返回第一个
	if lastUsedID == 0 {
		return &sortedAccounts[0]
	}

	// 找比 lastUsedID 大的第一个
	for i := range sortedAccounts {
		if sortedAccounts[i].ID > lastUsedID {
			return &sortedAccounts[i]
		}
	}

	// 没有比 lastUsedID 大的，绕回头部返回第一个
	return &sortedAccounts[0]
}

func (s *GroupLeakScanService) searchGitHub(ctx context.Context, account *models.GitHubSearchAccount, query, sourceType string, page int) ([]leakSearchResult, int64, error) {
	if sourceType == models.SearchAccountTypeGitHubAPI {
		return s.searchGitHubAPI(ctx, account, query, page)
	}
	return s.searchGitHubWeb(ctx, account, query, page)
}

func (s *GroupLeakScanService) searchGitHubAPI(ctx context.Context, account *models.GitHubSearchAccount, query string, page int) ([]leakSearchResult, int64, error) {
	endpoint := "https://api.github.com/search/code?q=" + url.QueryEscape(query) + fmt.Sprintf("&sort=indexed&order=desc&per_page=%d&page=%d", githubAPISearchPerPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+account.Credential)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", pickSearchAccountUserAgent(account.DeviceID))
	resp, err := s.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("github api search failed: status %d, body: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		TotalCount int64  `json:"total_count"`
		Items      []struct {
			HTMLURL string `json:"html_url"`
			URL     string `json:"url"`
		} `json:"items"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, err
	}
	if payload.Message != "" {
		return nil, 0, fmt.Errorf("github api search error: %s", payload.Message)
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
	if payload.AccountStrategy == "" || payload.AccountStrategy == "random" {
		payload.AccountStrategy = models.LeakScanAccountStrategyRoundRobin
	}
	return payload, nil
}

// preprocessQueryForAPI 将正则查询转换为 GitHub REST API 兼容的关键词查询。
// GitHub REST API 不支持正则语法 /pattern/，也不支持 content: 运算符。
// 会移除 content: 运算符，提取正则中的固定字符串作为关键词。
// 例如: /nvapi-[a-zA-Z0-9_\-]{64}/ AND content:"nvidia" → "nvapi-"
func preprocessQueryForAPI(query string) string {
	if query == "" {
		return query
	}

	// 按 AND/OR/NOT 分割
	parts := splitQueryParts(query)
	var results []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 已经是引号包裹的字符串，保留
		if strings.HasPrefix(part, "\"") && strings.HasSuffix(part, "\"") {
			results = append(results, part)
			continue
		}

		// API 不支持的运算符（content:, in:file 等），提取其中的引号内容作为关键词
		// content:"nvidia" → "nvidia"
		if regexp.MustCompile(`^[a-zA-Z]+:`).MatchString(part) {
			// 提取引号中的内容
			quoted := regexp.MustCompile(`"([^"]+)"`).FindStringSubmatch(part)
			if len(quoted) >= 2 && len(quoted[1]) >= 3 {
				results = append(results, "\""+quoted[1]+"\"")
			}
			continue
		}

		// 正则模式 /pattern/，提取固定字符串
		if strings.HasPrefix(part, "/") && strings.HasSuffix(part, "/") && len(part) > 2 {
			pattern := part[1 : len(part)-1]
			fixedStrings := extractFixedStringsFromRegex(pattern)
			for _, s := range fixedStrings {
				if len(s) >= 3 {
					results = append(results, "\""+s+"\"")
				}
			}
			continue
		}

		// 非正则非运算符的普通字符串，加引号保留
		results = append(results, "\""+part+"\"")
	}

	if len(results) == 0 {
		return query
	}
	return strings.Join(results, " AND ")
}

// splitQueryParts 按 AND/OR 分割查询字符串
func splitQueryParts(query string) []string {
	// 简单按 " AND " 分割，同时处理 " OR "
	parts := strings.FieldsFunc(query, func(r rune) bool {
		return false
	})
	// 先按 AND 分割
	andParts := strings.Split(query, " AND ")
	var result []string
	for _, p := range andParts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 再按 OR 分割
		orParts := strings.Split(p, " OR ")
		result = append(result, orParts...)
	}
	_ = parts
	return result
}

// extractFixedStringsFromRegex 从正则表达式中提取固定字符串部分
// 例如: nvapi-[a-zA-Z0-9_\-]{64} → ["nvapi-"]
func extractFixedStringsFromRegex(pattern string) []string {
	var result []string
	var current strings.Builder

	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]

		// 遇到字符类 [ 开始，先保存当前固定字符串
		if ch == '[' {
			if current.Len() > 0 {
				s := current.String()
				// 去掉转义反斜杠
				s = strings.ReplaceAll(s, "\\", "")
				if s != "" {
					result = append(result, s)
				}
				current.Reset()
			}
			// 跳过字符类 [...] 和后续的 {n}
			i = skipCharClass(pattern, i)
			continue
		}

		// 遇到量词 {，跳过
		if ch == '{' {
			i = skipQuantifier(pattern, i)
			continue
		}

		// 遇到分组 (，跳过
		if ch == '(' {
			if current.Len() > 0 {
				s := current.String()
				s = strings.ReplaceAll(s, "\\", "")
				if s != "" {
					result = append(result, s)
				}
				current.Reset()
			}
			i = skipGroup(pattern, i)
			continue
		}

		// 遇到锚点 ^ $，跳过
		if ch == '^' || ch == '$' {
			continue
		}

		// 遇到转义字符
		if ch == '\\' && i+1 < len(pattern) {
			current.WriteByte(pattern[i+1])
			i++
			continue
		}

		// 遇到通配符 . * + ?，结束当前固定字符串
		if ch == '.' || ch == '*' || ch == '+' || ch == '?' {
			if current.Len() > 0 {
				s := current.String()
				s = strings.ReplaceAll(s, "\\", "")
				if s != "" {
					result = append(result, s)
				}
				current.Reset()
			}
			continue
		}

		// 普通字符
		current.WriteByte(ch)
	}

	// 保存最后的固定字符串
	if current.Len() > 0 {
		s := current.String()
		s = strings.ReplaceAll(s, "\\", "")
		if s != "" {
			result = append(result, s)
		}
	}

	return result
}

// skipCharClass 跳过字符类 [...] 及后续的 {n} 量词，返回结束位置
func skipCharClass(pattern string, start int) int {
	i := start + 1
	// 跳过到 ]
	for i < len(pattern) {
		if pattern[i] == ']' && pattern[i-1] != '\\' {
			break
		}
		i++
	}
	i++ // 跳过 ]
	// 跳过后续的 {n} 或 {n,m}
	if i < len(pattern) && pattern[i] == '{' {
		i = skipQuantifier(pattern, i)
	}
	return i
}

// skipQuantifier 跳过 {n} 或 {n,m} 量词，返回结束位置
func skipQuantifier(pattern string, start int) int {
	i := start + 1
	for i < len(pattern) && pattern[i] != '}' {
		i++
	}
	if i < len(pattern) {
		i++ // 跳过 }
	}
	return i
}

// skipGroup 跳过 (...) 分组，返回结束位置
func skipGroup(pattern string, start int) int {
	depth := 1
	i := start + 1
	for i < len(pattern) && depth > 0 {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			i += 2
			continue
		}
		if pattern[i] == '(' {
			depth++
		}
		if pattern[i] == ')' {
			depth--
		}
		i++
	}
	return i
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

func effectivePerPage(sourceType string) int {
	if sourceType == models.SearchAccountTypeGitHubAPI {
		return githubAPISearchPerPage
	}
	return githubWebSearchPerPage
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
	delete(s.stopRequested, groupID)
	s.mu.Unlock()
}
