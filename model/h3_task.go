package model

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var (
	ErrH3IdempotencyConflict = errors.New("H3 idempotency key was already used for another request")
	ErrH3InsufficientQuota   = errors.New("insufficient wallet or token quota for H3 video")
	ErrH3InvalidReservation  = errors.New("invalid H3 wallet reservation")
	ErrH3TaskTerminal        = errors.New("successful H3 task cannot be refunded")
	ErrH3UnsafeBillingMode   = errors.New("H3 durable billing requires REDIS_CONN_STRING unset and BATCH_UPDATE_ENABLED=false")
)

// H3TaskContext is the frozen outbox, private to the gateway.
type H3TaskContext struct {
	BillingExpr string          `json:"billing_expr"`
	RequestBody json.RawMessage `json:"request_body"`
	Seconds     int             `json:"seconds"`
	Size        string          `json:"size"`
	BaseURL     string          `json:"base_url"`
	Proxy       string          `json:"proxy,omitempty"`
}

// H3TaskReservation is the durable billing guard for an H3 task. TaskID is
// derived from the authenticated user/token and client idempotency key; unlike
// the legacy tasks.task_id index, this primary key enforces uniqueness in DB.
type H3TaskReservation struct {
	TaskID      string `gorm:"primaryKey;type:varchar(191)"`
	UserId      int
	TokenId     int
	RequestHash string `gorm:"type:varchar(128)"`
	Quota       int
	Refunded    bool
}

// ReserveH3Task persists the replayable task and debits wallet/token balances in
// one transaction, before any upstream request. The boolean reports a new
// reservation; replaying an identical request returns the stored task unchanged.
func ReserveH3Task(task *Task, requestHash string) (*Task, bool, error) {
	if task == nil || task.ID != 0 || task.TaskID == "" || len(task.TaskID) > 191 ||
		task.UserId <= 0 || task.PrivateData.TokenId <= 0 || task.ChannelId <= 0 ||
		task.Quota <= 0 || int64(task.Quota) > math.MaxInt32 || requestHash == "" || len(requestHash) > 128 ||
		task.PrivateData.BillingSource != "wallet" || task.PrivateData.SubscriptionId != 0 {
		return nil, false, ErrH3InvalidReservation
	}

	var result *Task
	var created bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		// Serialize requests from the same wallet on MySQL/PostgreSQL. The
		// conditional debit below also protects SQLite and other billing paths.
		var user User
		if err := lockForUpdate(tx).Where("id = ?", task.UserId).First(&user).Error; err != nil {
			return err
		}
		var reservation H3TaskReservation
		err := lockForUpdate(tx).Where("task_id = ?", task.TaskID).First(&reservation).Error
		if err == nil {
			if reservation.UserId != task.UserId || reservation.TokenId != task.PrivateData.TokenId || reservation.RequestHash != requestHash {
				return ErrH3IdempotencyConflict
			}
			var stored Task
			if err := tx.Where("user_id = ? AND task_id = ?", task.UserId, task.TaskID).First(&stored).Error; err != nil {
				return err
			}
			result = &stored
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// A database transaction cannot see queued cache-first debits from the
		// generic billing path. Never create a reservation from a stale balance.
		if common.RedisEnabled || common.BatchUpdateEnabled {
			return ErrH3UnsafeBillingMode
		}
		if user.Status != common.UserStatusEnabled {
			return ErrH3InvalidReservation
		}
		if user.Quota < task.Quota {
			return ErrH3InsufficientQuota
		}

		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.PrivateData.TokenId, task.UserId).First(&token).Error; err != nil {
			return err
		}
		if token.Status != common.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix()) {
			return ErrH3InvalidReservation
		}
		if !token.UnlimitedQuota && token.RemainQuota < task.Quota {
			return ErrH3InsufficientQuota
		}

		reservation = H3TaskReservation{TaskID: task.TaskID, UserId: task.UserId, TokenId: token.Id, RequestHash: requestHash, Quota: task.Quota}
		if err := tx.Create(&reservation).Error; err != nil {
			return err
		}
		debit := tx.Model(&User{}).Where("id = ? AND quota >= ?", user.Id, task.Quota).Updates(map[string]any{
			"quota":         gorm.Expr("quota - ?", task.Quota),
			"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
			"request_count": gorm.Expr("request_count + 1"),
		})
		if debit.Error != nil {
			return debit.Error
		}
		if debit.RowsAffected != 1 {
			return ErrH3InsufficientQuota
		}
		tokenDebit := tx.Model(&Token{}).Where("id = ? AND user_id = ?", token.Id, user.Id)
		if !token.UnlimitedQuota {
			tokenDebit = tokenDebit.Where("remain_quota >= ?", task.Quota)
		}
		debit = tokenDebit.Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", task.Quota),
			"used_quota":    gorm.Expr("used_quota + ?", task.Quota),
			"accessed_time": time.Now().Unix(),
		})
		if debit.Error != nil {
			return debit.Error
		}
		if debit.RowsAffected != 1 {
			return ErrH3InsufficientQuota
		}
		channelDebit := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", task.Quota))
		if channelDebit.Error != nil {
			return channelDebit.Error
		}
		if channelDebit.RowsAffected != 1 {
			return ErrH3InvalidReservation
		}
		if err := tx.Create(task).Error; err != nil {
			return err
		}
		created = true
		result = task
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

// RefundH3Task transitions a proven failed task and returns its frozen charge in
// the same transaction. The durable reservation guard survives process crashes
// and repeated polling; a completed task can never become a refunded failure.
func RefundH3Task(taskID, reason string) (*Task, bool, error) {
	var reservation H3TaskReservation
	if err := DB.Where("task_id = ?", taskID).First(&reservation).Error; err != nil {
		return nil, false, err
	}
	var task Task
	var refunded bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Unscoped().Where("id = ?", reservation.UserId).First(&user).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("task_id = ?", taskID).First(&reservation).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("task_id = ? AND user_id = ?", taskID, reservation.UserId).First(&task).Error; err != nil {
			return err
		}
		if reservation.Refunded {
			return nil
		}
		if task.Status == TaskStatusSuccess {
			return ErrH3TaskTerminal
		}
		if common.RedisEnabled || common.BatchUpdateEnabled {
			return ErrH3UnsafeBillingMode
		}
		if reservation.Quota <= 0 || reservation.Quota != task.Quota || task.PrivateData.TokenId != reservation.TokenId {
			return ErrH3InvalidReservation
		}
		quota := reservation.Quota
		if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
			"quota":      gorm.Expr("quota + ?", quota),
			"used_quota": gorm.Expr("used_quota - ?", quota),
		}).Error; err != nil {
			return err
		}
		// A soft-deleted token still receives its refund; deleting a credential
		// does not erase the durable wallet charge or block the user's refund.
		var token Token
		err := lockForUpdate(tx).Unscoped().Where("id = ? AND user_id = ?", reservation.TokenId, user.Id).First(&token).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			if err := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{
				"remain_quota": gorm.Expr("remain_quota + ?", quota),
				"used_quota":   gorm.Expr("used_quota - ?", quota),
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&Channel{}).Where("id = ?", task.ChannelId).Update("used_quota", gorm.Expr("used_quota - ?", quota)).Error; err != nil {
			return err
		}
		task.Status = TaskStatusFailure
		task.Progress = "100%"
		task.FailReason = reason
		task.FinishTime = time.Now().Unix()
		if err := tx.Model(&task).Select("status", "progress", "fail_reason", "finish_time").Updates(&task).Error; err != nil {
			return err
		}
		if err := tx.Model(&reservation).Update("refunded", true).Error; err != nil {
			return err
		}
		refunded = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &task, refunded, nil
}
