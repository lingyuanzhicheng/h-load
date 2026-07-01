package handler

import (
	"context"
	"strconv"

	app_errors "h-load/internal/errors"
	"h-load/internal/models"
	"h-load/internal/response"
	"h-load/internal/services"

	"github.com/gin-gonic/gin"
)

func (s *Server) ListSearchAccounts(c *gin.Context) {
	accounts, err := s.SearchAccountService.List(c.Request.Context(), c.Query("type"), c.Query("status"))
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, accounts)
}

func (s *Server) CreateSearchAccount(c *gin.Context) {
	var account models.GitHubSearchAccount
	if err := c.ShouldBindJSON(&account); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}
	created, err := s.SearchAccountService.Create(c.Request.Context(), &account)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, created)
}

func (s *Server) UpdateSearchAccount(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var updates map[string]any
	if err := c.ShouldBindJSON(&updates); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}
	account, err := s.SearchAccountService.Update(c.Request.Context(), id, updates)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, account)
}

func (s *Server) DeleteSearchAccount(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	if err := s.SearchAccountService.Delete(c.Request.Context(), id); err != nil {
		s.handleGroupError(c, err)
		return
	}
	response.Success(c, nil)
}

func (s *Server) ValidateSearchAccount(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	account, err := s.SearchAccountService.Validate(c.Request.Context(), id)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, account)
}

func (s *Server) ValidateSearchAccounts(c *gin.Context) {
	valid, invalid, err := s.SearchAccountService.ValidateMany(c.Request.Context(), c.Query("type"), c.Query("status"))
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, gin.H{"valid": valid, "invalid": invalid})
}

func (s *Server) ClearSearchAccountsByStatus(c *gin.Context) {
	var payload struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}
	if payload.Status != models.SearchAccountStatusActive && payload.Status != models.SearchAccountStatusInactive && payload.Status != models.SearchAccountStatusLimited {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid status"))
		return
	}
	deleted, err := s.SearchAccountService.ClearByStatus(c.Request.Context(), payload.Status)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func (s *Server) GetLeakScanConfig(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	cfg, err := s.GroupLeakScanService.GetConfig(c.Request.Context(), groupID)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, leakScanConfigResponse(cfg))
}

func (s *Server) SaveLeakScanConfig(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var payload services.LeakScanConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInvalidJSON, err.Error()))
		return
	}
	cfg, err := s.GroupLeakScanService.SaveConfig(c.Request.Context(), groupID, payload)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, leakScanConfigResponse(cfg))
}

func (s *Server) GetLeakScanStatus(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	status, err := s.GroupLeakScanService.GetStatus(c.Request.Context(), groupID)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, gin.H{"config": leakScanConfigResponse(status.Config), "run": status.Run})
}

func (s *Server) StartLeakScan(c *gin.Context) {
	s.runLeakScanAction(c, s.GroupLeakScanService.Start)
}

func (s *Server) StopLeakScan(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	if err := s.GroupLeakScanService.Stop(c.Request.Context(), groupID); err != nil {
		s.handleGroupError(c, err)
		return
	}
	response.Success(c, nil)
}

func (s *Server) ResumeLeakScan(c *gin.Context) {
	s.runLeakScanAction(c, s.GroupLeakScanService.Resume)
}

func (s *Server) ResetLeakScan(c *gin.Context) {
	s.runLeakScanAction(c, s.GroupLeakScanService.Reset)
}

func (s *Server) InitializeLeakScan(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	if err := s.GroupLeakScanService.Initialize(c.Request.Context(), groupID); err != nil {
		s.handleGroupError(c, err)
		return
	}
	response.Success(c, nil)
}

func (s *Server) ListLeakScanRuns(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	runs, total, err := s.GroupLeakScanService.ListRuns(c.Request.Context(), groupID, page, pageSize)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, gin.H{"items": runs, "pagination": pagination(page, pageSize, total)})
}

func (s *Server) ListLeakScanEvents(c *gin.Context) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	runID64, _ := strconv.ParseUint(c.Query("run_id"), 10, 32)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	res, err := s.GroupLeakScanService.ListEvents(c.Request.Context(), groupID, uint(runID64), page, pageSize)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, res)
}

func (s *Server) runLeakScanAction(c *gin.Context, action func(context.Context, uint) (*models.GroupLeakScanRun, error)) {
	groupID, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	run, err := action(c.Request.Context(), groupID)
	if s.handleGroupError(c, err) {
		return
	}
	response.Success(c, run)
}

func parseUintParam(c *gin.Context, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "invalid id"))
		return 0, false
	}
	return uint(id), true
}

func pagination(page, pageSize int, total int64) gin.H {
	if pageSize <= 0 {
		pageSize = 20
	}
	return gin.H{"page": page, "page_size": pageSize, "total_items": total, "total_pages": (total + int64(pageSize) - 1) / int64(pageSize)}
}

func leakScanConfigResponse(cfg *models.GroupLeakScanConfig) services.LeakScanConfigPayload {
	if cfg == nil {
		return services.LeakScanConfigPayload{}
	}
	payload, _ := services.LeakScanConfigFromModel(cfg)
	return payload
}
