package tray

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
)

type PolicySettings struct {
	Available    bool
	Repairable   bool
	Revision     string
	Enabled      bool
	Interval     string
	StableFor    string
	ArchivedOnly bool
	BatchSize    int
	Error        string
}

type PolicyChange struct {
	Action   string          `json:"action"`
	ID       string          `json:"id"`
	Revision string          `json:"revision"`
	Field    string          `json:"field"`
	Value    json.RawMessage `json:"value"`
}

type ActionResult struct {
	ID      string
	OK      bool
	Message string
}

func marshalActionResult(result ActionResult) ([]byte, error) { return json.Marshal(result) }

func decodePolicyChange(message string) (PolicyChange, error) {
	if len(message) > 4096 {
		return PolicyChange{}, errors.New("设置请求过长")
	}
	var request PolicyChange
	decoder := json.NewDecoder(bytes.NewBufferString(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("设置请求格式有误")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || request.Action != "set-policy" || request.ID == "" || len(request.ID) > 64 {
		return request, errors.New("设置请求格式有误")
	}
	return request, nil
}

func (m *Monitor) policy() (string, enroll.Control, PolicySettings, error) {
	path := enroll.WorkerControlPath(m.Store)
	control, revision, err := enroll.LoadControlRevision(path)
	_, workerProgressErr := os.Lstat(enroll.WorkerProgressPath(m.Store))
	if revision == "" && err == nil && errors.Is(workerProgressErr, os.ErrNotExist) {
		path = enroll.ControlPath(m.Store)
		control, revision, err = enroll.LoadControlRevision(path)
	}
	settings := PolicySettings{Available: revision != "", Revision: revision,
		Enabled: control.Enabled, Interval: control.Interval.String(), StableFor: control.StableFor.String(),
		ArchivedOnly: control.ArchivedOnly, BatchSize: control.BatchSize}
	if err != nil {
		settings.Error = "自动折叠设置无法读取；可修复为暂停状态，再重新设置。"
		settings.Repairable = revision != ""
	} else if !settings.Available {
		settings.Error = "尚未找到自动折叠设置，请先安装后台服务。"
		progressPath := enroll.ProgressPath(m.Store)
		if path == enroll.WorkerControlPath(m.Store) {
			progressPath = enroll.WorkerProgressPath(m.Store)
		}
		if _, progressErr := os.Lstat(progressPath); progressErr == nil {
			settings.Repairable = true
			settings.Error = "自动折叠设置缺失，可修复为暂停状态。"
			if path == enroll.ControlPath(m.Store) {
				settings.Error = "后台使用启动参数；可建立策略文件并保持暂停。"
			}
		}
	}
	return path, control, settings, err
}

// ApplyPolicy changes one selected field. Custom values in the other fields
// survive GUI edits, and a stale GUI cannot overwrite newly installed settings.
func (m *Monitor) ApplyPolicy(request PolicyChange) ActionResult {
	result := ActionResult{ID: request.ID}
	path, control, settings, _ := m.policy()
	if request.Revision != settings.Revision || !(settings.Available && request.Revision != "" || request.Field == "reset" && settings.Repairable) {
		result.Message = "设置已变化，请等待刷新后重试。"
		return result
	}
	if settings.Error != "" && request.Field != "reset" {
		result.Message = settings.Error
		return result
	}
	var err error
	if request.Field != "reset" && (len(request.Value) == 0 || bytes.Equal(bytes.TrimSpace(request.Value), []byte("null"))) {
		result.Message = "未保存：设置值不能为空。"
		return result
	}
	switch request.Field {
	case "enabled":
		err = json.Unmarshal(request.Value, &control.Enabled)
		if err == nil && control.Enabled && control.Interval < 30*time.Second {
			err = errors.New("请先把检查间隔设为至少 30 秒")
		}
	case "interval", "stable_for":
		var raw string
		var value time.Duration
		err = json.Unmarshal(request.Value, &raw)
		if err == nil {
			value, err = time.ParseDuration(raw)
		}
		if request.Field == "interval" {
			if err == nil && (value < 30*time.Second || value > 24*time.Hour) {
				err = errors.New("检查间隔须在 30 秒到 24 小时之间")
			}
			control.Interval = value
		} else {
			if err == nil && (value < time.Minute || value > 30*24*time.Hour) {
				err = errors.New("空闲时间须在 1 分钟到 30 天之间")
			}
			control.StableFor = value
		}
	case "archived_only":
		err = json.Unmarshal(request.Value, &control.ArchivedOnly)
	case "batch_size":
		err = json.Unmarshal(request.Value, &control.BatchSize)
		if err == nil && (control.BatchSize < 0 || control.BatchSize > 1000) {
			err = errors.New("每批会话数须为自动或 1 到 1000")
		}
	case "reset":
		if settings.Error == "" {
			err = errors.New("当前设置有效，无需修复")
		} else {
			control = enroll.Control{Present: true, Interval: 5 * time.Minute, StableFor: time.Hour, ArchivedOnly: true}
		}
	default:
		err = errors.New("不支持的设置项")
	}
	if err != nil {
		result.Message = "未保存：" + err.Error()
		return result
	}
	// Check once more immediately before the atomic replacement. The worker only
	// reads this file; changes from another UI or installer remain visible here.
	_, latestRevision, loadErr := enroll.LoadControlRevision(path)
	if latestRevision != request.Revision || loadErr != nil && request.Field != "reset" {
		result.Message = "设置已变化，请等待刷新后重试。"
		return result
	}
	if err := enroll.SaveControl(path, control); err != nil {
		result.Message = fmt.Sprintf("设置未保存，请检查目录权限：%v", err)
		return result
	}
	result.OK = true
	result.Message = "已保存，新设置将由后台自动应用。"
	if request.Field == "enabled" && !control.Enabled {
		result.Message = "已请求暂停，等待当前操作安全退出。"
	}
	if request.Field == "reset" {
		result.Message = "设置已修复，自动折叠保持暂停。"
	}
	return result
}
