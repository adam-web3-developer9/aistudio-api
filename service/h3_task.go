package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

type H3InputError struct {
	Status int
	Code   string
}

func (e *H3InputError) Error() string { return "H3 upload receipt was rejected: " + e.Code }

// SyncH3Task reconciles a durable outbox against the same operator credential
// and request key. Transport errors leave both the reservation and task pending.
func SyncH3Task(ctx context.Context, task *model.Task) error {
	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	h3 := task.PrivateData.H3
	if h3 == nil || len(h3.RequestBody) == 0 || h3.BaseURL == "" || task.PrivateData.Key == "" {
		return fmt.Errorf("H3 outbox context is missing")
	}
	if GetTaskAdaptorFunc == nil {
		return fmt.Errorf("H3 adaptor factory is not initialized")
	}
	adaptor := GetTaskAdaptorFunc(constant.TaskPlatformH3Studio)
	if adaptor == nil {
		return fmt.Errorf("H3 adaptor is not initialized")
	}
	resp, err := adaptor.FetchTask(h3.BaseURL, task.PrivateData.Key, task, h3.Proxy)
	if err != nil {
		if rejection, ok := err.(interface{ DefinitiveRejection() bool }); ok && rejection.DefinitiveRejection() {
			return refundH3Task(task, "H3 upstream rejected the frozen request before accepting a task")
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("H3 status lookup returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return fmt.Errorf("H3 response is unreadable or exceeds the metadata limit")
	}
	var envelope struct {
		Billing *struct {
			Unit             string `json:"unit"`
			RequestedSeconds int    `json:"requestedSeconds"`
		} `json:"billing"`
	}
	if err := common.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("invalid H3 task response: %w", err)
	}
	if envelope.Billing == nil || envelope.Billing.Unit != "second" || envelope.Billing.RequestedSeconds != h3.Seconds {
		return fmt.Errorf("H3 task duration does not match its frozen billing snapshot")
	}
	result, err := adaptor.ParseTaskResult(task, resp, body)
	if err != nil {
		return err
	}
	sanitizer, ok := adaptor.(interface{ SanitizeTaskData([]byte) ([]byte, error) })
	if !ok {
		return fmt.Errorf("H3 response sanitizer is missing")
	}
	data, err := sanitizer.SanitizeTaskData(body)
	if err != nil {
		return err
	}
	if result.Status == model.TaskStatusFailure {
		return refundH3Task(task, result.Reason)
	}
	snap := task.Snapshot()
	task.Data = data
	task.Status = model.TaskStatus(result.Status)
	task.Progress = result.Progress
	switch task.Status {
	case model.TaskStatusSubmitted, model.TaskStatusQueued:
	case model.TaskStatusInProgress:
		if task.StartTime == 0 {
			task.StartTime = time.Now().Unix()
		}
	case model.TaskStatusSuccess:
		if result.Url == "" {
			return fmt.Errorf("H3 completed without an archived result")
		}
		task.PrivateData.ResultURL = result.Url
		task.Progress = "100%"
		task.FinishTime = time.Now().Unix()
	default:
		return fmt.Errorf("unknown H3 status %s", result.Status)
	}
	if _, err := task.UpdateWithStatus(snap.Status); err != nil {
		return err
	}
	// Return the winning projection when another process completed/refunded it.
	stored, exists, err := model.GetByTaskId(task.UserId, task.TaskID)
	if err != nil {
		return err
	}
	if exists {
		*task = *stored
	}
	return nil
}

func refundH3Task(task *model.Task, reason string) error {
	refundedTask, refunded, err := model.RefundH3Task(task.TaskID, reason)
	if err != nil {
		return err
	}
	*task = *refundedTask
	if refunded {
		other := taskBillingOther(task)
		other.SetPublic("task_id", task.TaskID)
		other.SetPublic("billing_unit", "second")
		other.SetPublic("reason", reason)
		model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
			UserId: task.UserId, LogType: model.LogTypeRefund, Content: []*common.Message{common.NewMessage("H3 confirmed failure refund")},
			ChannelId: task.ChannelId, ModelName: task.Properties.OriginModelName, Quota: task.Quota,
			TokenId: task.PrivateData.TokenId, Group: task.Group, Other: other, NodeName: task.PrivateData.NodeName,
		})
	}
	return nil
}

// SubmitH3Inputs relays only reservation-bound input receipts to Studio.
func SubmitH3Inputs(ctx context.Context, task *model.Task, body []byte) error {
	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure || task.PrivateData.H3 == nil {
		return fmt.Errorf("H3 task no longer accepts upload receipts")
	}
	h3 := task.PrivateData.H3
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := GetHttpClientWithProxy(h3.Proxy)
	if err != nil {
		return err
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	lookup, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h3.BaseURL, "/")+"/v1/jobs/by-request-key/"+url.PathEscape(task.TaskID), nil)
	if err != nil {
		return err
	}
	lookup.Header.Set("x-api-key", task.PrivateData.Key)
	resp, err := boundedClient.Do(lookup)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("H3 upload target lookup returned HTTP %d", resp.StatusCode)
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := common.DecodeJson(io.LimitReader(resp.Body, 2*1024*1024), &job); err != nil || job.ID == "" {
		return fmt.Errorf("invalid H3 upload target response")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h3.BaseURL, "/")+"/v1/jobs/"+url.PathEscape(job.ID)+"/inputs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", task.PrivateData.Key)
	inputResp, err := boundedClient.Do(req)
	if err != nil {
		return err
	}
	defer inputResp.Body.Close()
	if inputResp.StatusCode != http.StatusOK && inputResp.StatusCode != http.StatusAccepted {
		var failure struct {
			Code  string `json:"code"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = common.DecodeJson(io.LimitReader(inputResp.Body, 64*1024), &failure)
		if failure.Error.Code == "" {
			failure.Error.Code = failure.Code
		}
		if inputResp.StatusCode == http.StatusConflict || inputResp.StatusCode == http.StatusBadRequest {
			code := "COMFY_UPLOAD_UNAVAILABLE"
			switch failure.Error.Code {
			case "COMFY_UPLOAD_RESERVATION_STALE", "COMFY_UPLOAD_INVALID", "COMFY_UPLOAD_UNAVAILABLE":
				code = failure.Error.Code
			}
			return &H3InputError{Status: inputResp.StatusCode, Code: code}
		}
		return fmt.Errorf("H3 upload receipt returned HTTP %d; refresh the task upload reservation", inputResp.StatusCode)
	}
	return SyncH3Task(ctx, task)
}
