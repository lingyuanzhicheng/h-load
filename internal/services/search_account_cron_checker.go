package services

import (
	"context"
	"h-load/internal/config"
	"h-load/internal/models"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// SearchAccountCronChecker 定期验证受限（limited）状态的账户。
type SearchAccountCronChecker struct {
	db              *gorm.DB
	settingsManager *config.SystemSettingsManager
	accountService  *SearchAccountService
	stopChan        chan struct{}
	wg              sync.WaitGroup
}

// NewSearchAccountCronChecker 创建账户定时验证器。
func NewSearchAccountCronChecker(db *gorm.DB, settingsManager *config.SystemSettingsManager, accountService *SearchAccountService) *SearchAccountCronChecker {
	return &SearchAccountCronChecker{
		db:              db,
		settingsManager: settingsManager,
		accountService:  accountService,
		stopChan:        make(chan struct{}),
	}
}

// Start 启动定时验证。
func (s *SearchAccountCronChecker) Start() {
	logrus.Debug("Starting SearchAccountCronChecker...")
	s.wg.Add(1)
	go s.runLoop()
}

// Stop 停止定时验证。
func (s *SearchAccountCronChecker) Stop(ctx context.Context) {
	close(s.stopChan)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logrus.Info("SearchAccountCronChecker stopped gracefully.")
	case <-ctx.Done():
		logrus.Warn("SearchAccountCronChecker stop timed out.")
	}
}

func (s *SearchAccountCronChecker) runLoop() {
	defer s.wg.Done()

	// 启动时立即执行一次
	s.validateLimitedAccounts()

	for {
		settings := s.settingsManager.GetSettings()
		interval := settings.AccountValidationIntervalMinutes
		if interval < 1 {
			interval = 60
		}
		ticker := time.NewTicker(time.Duration(interval) * time.Minute)

		select {
		case <-ticker.C:
			ticker.Stop()
			logrus.Debug("SearchAccountCronChecker: Running periodic validation of limited accounts.")
			s.validateLimitedAccounts()
		case <-s.stopChan:
			ticker.Stop()
			return
		}
	}
}

// validateLimitedAccounts 查询所有 limited 账户并并发验证。
func (s *SearchAccountCronChecker) validateLimitedAccounts() {
	settings := s.settingsManager.GetSettings()

	var accounts []models.GitHubSearchAccount
	if err := s.db.Where("status = ?", models.SearchAccountStatusLimited).Find(&accounts).Error; err != nil {
		logrus.Errorf("SearchAccountCronChecker: Failed to get limited accounts: %v", err)
		return
	}

	if len(accounts) == 0 {
		logrus.Debug("SearchAccountCronChecker: No limited accounts to validate.")
		return
	}

	concurrency := settings.AccountValidationConcurrency
	if concurrency < 1 {
		concurrency = 1
	}

	var becameValidCount int32
	var wg sync.WaitGroup
	jobs := make(chan *models.GitHubSearchAccount, len(accounts))

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case account, ok := <-jobs:
					if !ok {
						return
					}
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					updated, _, err := s.accountService.Validate(ctx, account.ID)
					cancel()
					if err != nil {
						logrus.WithError(err).WithField("account_id", account.ID).Error("SearchAccountCronChecker: Failed to validate account")
						continue
					}
					if updated.Status == models.SearchAccountStatusActive {
						atomic.AddInt32(&becameValidCount, 1)
					}
				case <-s.stopChan:
					return
				}
			}
		}()
	}

DistributeLoop:
	for i := range accounts {
		select {
		case jobs <- &accounts[i]:
		case <-s.stopChan:
			break DistributeLoop
		}
	}
	close(jobs)
	wg.Wait()

	logrus.Infof(
		"SearchAccountCronChecker: Validation finished. Total checked: %d, became valid: %d.",
		len(accounts),
		becameValidCount,
	)
}
