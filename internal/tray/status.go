// Package tray implements the Windows status and automatic-folding companion.
package tray

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/fskitstatus"
)

type View struct {
	Health, Storage, Activity, Sessions, Detail            string
	Healthy                                                bool
	StorePath                                              string
	LogicalBytes, PhysicalBytes                            *int64
	CompressionLogicalBytes, CompressedBytes, PendingBytes *int64
	ReadRate, WriteRate                                    *float64
	DaemonState, ManagedState, EnrollmentState             string
	Settings                                               PolicySettings
	Progress                                               *EnrollmentProgress
	HistorySamples                                         []HistorySample
	Incidents                                              []Incident
	HistoryError                                           string
	Operation                                              *ActionResult
	LogDirectories                                         map[string]string
}

type EnrollmentProgress struct {
	Phase                      string
	ManagedCount, WaitingCount int
	WaitingKnown               bool
	CycleDone, CycleTotal      int
	NextCheckAt, UpdatedAt     string
	ErrorKind, LastError       string
	Fresh                      bool
}

type observation struct {
	snapshot fskitstatus.Snapshot
	advanced time.Time
}

type Monitor struct {
	Store     string
	last      map[string]observation
	connected bool
	space     accountingCache
	history   historyState
	logs      map[string]string
}

func NewMonitor(store string) *Monitor {
	return &Monitor{Store: store, last: make(map[string]observation), history: loadHistory(store, time.Now()), logs: logDirectories(store)}
}

func (m *Monitor) read(component string, now time.Time, maxAge time.Duration) (fskitstatus.Snapshot, error) {
	s, err := fskitstatus.Read(filepath.Join(m.Store, "fs", "status", component+".json"))
	if err != nil {
		return s, err
	}
	if s.Component != component {
		return s, errors.New("状态组件不匹配")
	}
	updated, err := time.Parse(time.RFC3339Nano, s.UpdatedAt)
	if err != nil || updated.After(now.Add(5*time.Second)) || now.Sub(updated) > maxAge {
		return s, errors.New("状态已过期，等待后台更新")
	}
	if component != "storage" {
		if s.PublisherInstanceID == "" || s.BackendID == "" || s.ObservationSequence == 0 {
			return s, errors.New("状态缺少运行实例或心跳序号")
		}
		previous := m.last[component]
		if s.PublisherInstanceID == previous.snapshot.PublisherInstanceID {
			if s.BackendID != previous.snapshot.BackendID || s.ObservationSequence < previous.snapshot.ObservationSequence {
				return s, errors.New("状态序号或后台标识发生异常变化")
			}
			if s.ObservationSequence == previous.snapshot.ObservationSequence {
				if now.Sub(previous.advanced) > maxAge {
					return s, errors.New("后台心跳已停止")
				}
				return s, nil
			}
		}
		m.last[component] = observation{s, now}
	}
	return s, nil
}

func (m *Monitor) Refresh(now time.Time) View {
	return m.refresh(now, true)
}

// DiagnosticSnapshot reads aggregate status without competing with the resident
// tray for ownership of the persistent history archive.
func (m *Monitor) DiagnosticSnapshot(now time.Time) View {
	return m.refresh(now, false)
}

func (m *Monitor) refresh(now time.Time, recordHistory bool) View {
	v := View{Health: "未连接", Storage: "空间统计：暂无数据", Activity: "读取 —    写入 —", Sessions: "托管会话：—", StorePath: m.Store, LogDirectories: m.logs}
	previous := m.last["daemon"].snapshot
	daemon, daemonErr := m.read("daemon", now, 15*time.Second)
	managed, managedErr := m.read("managed", now, 15*time.Second)
	v.DaemonState = componentState(daemon, daemonErr)
	v.ManagedState = componentState(managed, managedErr)
	v.EnrollmentState = "未连接"
	policyPath, policy, settings, policyErr := m.policy()
	v.Settings = settings
	usingWorker := policyPath == enroll.WorkerControlPath(m.Store)
	if settings.Available || settings.Repairable || policyErr != nil || daemonErr == nil && daemon.State == "healthy" {
		progressPath := enroll.ProgressPath(m.Store)
		progressMaxAge := 15 * time.Minute
		if usingWorker {
			progressPath = enroll.WorkerProgressPath(m.Store)
			progressMaxAge = 30 * time.Second
		}
		progress, err := enroll.LoadProgress(progressPath)
		if policyErr != nil {
			err = policyErr
		}
		fresh := err == nil && !progress.UpdatedAt.IsZero() && !progress.UpdatedAt.After(now.Add(5*time.Second)) && now.Sub(progress.UpdatedAt) <= progressMaxAge
		v.Progress = &EnrollmentProgress{Phase: progress.Phase, ManagedCount: progress.ManagedCount,
			WaitingCount: progress.WaitingCount, WaitingKnown: progress.WaitingKnown, CycleDone: progress.CycleDone,
			CycleTotal: progress.CycleTotal, ErrorKind: progress.ErrorKind, LastError: progress.LastError, Fresh: fresh}
		if !progress.UpdatedAt.IsZero() {
			v.Progress.UpdatedAt = progress.UpdatedAt.Format(time.RFC3339Nano)
		}
		if !progress.NextCheckAt.IsZero() {
			v.Progress.NextCheckAt = progress.NextCheckAt.Format(time.RFC3339Nano)
		}
		switch {
		case policyErr != nil:
			v.EnrollmentState = "设置有误"
		case err != nil:
			v.EnrollmentState = "状态不可用"
		case progress.UpdatedAt.IsZero():
			v.EnrollmentState = "暂无状态"
		case progress.UpdatedAt.After(now.Add(5*time.Second)) || now.Sub(progress.UpdatedAt) > progressMaxAge:
			v.EnrollmentState = "状态已过期"
		case progress.Phase == enroll.PhaseConfigInvalid:
			v.EnrollmentState = "设置有误"
		case progress.Phase == enroll.PhaseStopped:
			v.EnrollmentState = "后台已停止"
		case progress.Phase == enroll.PhaseWaitingFilesystem:
			v.EnrollmentState = "等待文件系统"
		case settings.Available && !policy.Enabled && progress.Enabled:
			v.EnrollmentState = "正在暂停"
		case progress.LastError != "":
			v.EnrollmentState = "需要关注"
		case !progress.Enabled:
			v.EnrollmentState = "已关闭"
		case daemonErr != nil || daemon.State != "healthy":
			v.EnrollmentState = "等待文件系统"
		default:
			v.EnrollmentState = map[string]string{
				enroll.PhaseIdle: "等待下次检查", enroll.PhaseChecking: "正在检查",
				enroll.PhaseFolding: "正在折叠", enroll.PhasePacking: "正在打包",
				enroll.PhaseMigrating: "正在迁移", enroll.PhaseReclaiming: "正在回收",
				enroll.PhaseWaitingReclaim: "等待回收",
			}[progress.Phase]
			if v.EnrollmentState == "" {
				v.EnrollmentState = "状态未知"
			}
		}
	}
	var details []string
	if errors.Is(daemonErr, os.ErrNotExist) && errors.Is(managedErr, os.ErrNotExist) && !m.connected {
		details = append(details, "尚未发现后台状态。请先在隔离目录中启动 WinFsp 文件服务。")
	} else {
		m.connected = true
		v.Health = "需要关注"
		if daemonErr == nil && managedErr == nil && daemon.State == "healthy" && managed.State == "healthy" {
			v.Health, v.Healthy = "运行正常", true
		} else if daemonErr == nil && (daemon.State == "starting" || daemon.State == "stopped") {
			v.Health = map[string]string{"starting": "正在启动", "stopped": "已停止"}[daemon.State]
		}
		for _, channel := range []struct {
			name     string
			snapshot fskitstatus.Snapshot
			err      error
		}{
			{"后台", daemon, daemonErr}, {"会话", managed, managedErr},
		} {
			if channel.err != nil {
				details = append(details, channel.name+"："+channel.err.Error())
			} else {
				details = append(details, channel.name+"："+channel.snapshot.State)
				if channel.snapshot.Detail != "" {
					details = append(details, channel.snapshot.Detail)
				}
			}
		}
	}
	if managedErr == nil && managed.ManagedSessions != nil {
		v.Sessions = fmt.Sprintf("托管会话：%d", *managed.ManagedSessions)
	}
	if daemonErr == nil && daemon.State == "healthy" && previous.State == "healthy" &&
		previous.PublisherInstanceID == daemon.PublisherInstanceID && previous.BackendID == daemon.BackendID &&
		daemon.ObservationSequence > previous.ObservationSequence &&
		daemon.ReadBytesTotal != nil && daemon.WrittenBytesTotal != nil && previous.ReadBytesTotal != nil && previous.WrittenBytesTotal != nil &&
		*daemon.ReadBytesTotal >= *previous.ReadBytesTotal && *daemon.WrittenBytesTotal >= *previous.WrittenBytesTotal {
		before, _ := time.Parse(time.RFC3339Nano, previous.UpdatedAt)
		after, _ := time.Parse(time.RFC3339Nano, daemon.UpdatedAt)
		if seconds := after.Sub(before).Seconds(); seconds > 0 && seconds <= 15 {
			readRate := float64(*daemon.ReadBytesTotal-*previous.ReadBytesTotal) / seconds
			writeRate := float64(*daemon.WrittenBytesTotal-*previous.WrittenBytesTotal) / seconds
			v.ReadRate, v.WriteRate = &readRate, &writeRate
			v.Activity = fmt.Sprintf("读取 %s/s    写入 %s/s", bytesText(readRate), bytesText(writeRate))
		}
	}
	storage, storageErr := m.read("storage", now, 15*time.Minute)
	if storageErr == nil && storage.State == "healthy" && storage.LogicalBytes != nil && storage.PhysicalBytes != nil && *storage.LogicalBytes >= 0 && *storage.PhysicalBytes >= 0 {
		logical, physical := float64(*storage.LogicalBytes), float64(*storage.PhysicalBytes)
		v.LogicalBytes, v.PhysicalBytes = storage.LogicalBytes, storage.PhysicalBytes
		saving := "暂无可比较数据"
		if logical > 0 {
			if physical <= logical {
				saving = fmt.Sprintf("净节省 %.1f%%", (logical-physical)/logical*100)
			} else {
				saving = "物理占用高于逻辑数据量"
			}
		}
		v.Storage = fmt.Sprintf("逻辑数据 %s    物理占用 %s\r\n%s", bytesText(logical), bytesText(physical), saving)
		details = append(details, "空间统计包含存储、保留副本和恢复文件；更新时间："+storage.UpdatedAt)
	} else if storageErr != nil && !errors.Is(storageErr, os.ErrNotExist) {
		details = append(details, "空间统计："+storageErr.Error())
	}
	if measured, err := m.accounting(now); measured != nil {
		v.LogicalBytes, v.PhysicalBytes = &measured.Logical, &measured.Physical
		v.CompressionLogicalBytes, v.CompressedBytes, v.PendingBytes = &measured.CompressionLogical, &measured.Compressed, &measured.Pending
		details = append(details, "压缩统计依据已发布压缩包；待回收占用包含保留原件、散对象、旧版本及恢复文件，实际回收需通过校验。更新时间："+measured.UpdatedAt.Format(time.RFC3339))
		if err != nil {
			details = append(details, "空间明细正在重试，暂显示最近一次有效统计。")
		}
	}
	if daemon.MountPoint != "" {
		details = append(details, "挂载目录："+daemon.MountPoint)
	}
	details = append(details, "存储目录："+m.Store, "Windows 预览版 · 退出此界面后后台服务继续运行。")
	v.Detail = strings.Join(details, "\r\n\r\n")
	var issue *runtimeIssue
	if m.connected || settings.Available || settings.Repairable {
		switch {
		case daemonErr != nil || daemon.State != "healthy":
			issue = &runtimeIssue{Source: "filesystem", Reason: "文件服务尚未就绪", Impact: "托管会话的访问可能暂时受影响。",
				Recommendations: []string{"等待 Windows 服务恢复后点击“检查状态”。", "持续未恢复时查看文件服务日志并导出诊断。"}}
			if daemonErr != nil {
				issue.Detail = daemonErr.Error()
			} else {
				issue.Detail = daemon.Detail
			}
		case managedErr != nil || managed.State != "healthy":
			issue = &runtimeIssue{Source: "managed", Reason: "会话管理状态异常", Impact: "托管会话的当前健康状态尚未得到确认。",
				Recommendations: []string{"稍后重新检查状态。", "持续异常时查看服务日志并导出诊断。"}, Detail: managed.Detail}
			if managedErr != nil {
				issue.Detail = managedErr.Error()
			}
		case policyErr != nil || usingWorker && settings.Repairable && settings.Error != "":
			issue = &runtimeIssue{Source: "enrollment", Reason: "自动折叠设置无效", Impact: "新的自动折叠工作暂停。",
				Recommendations: []string{"点击“修复设置”，然后选择所需参数。"}, Detail: settings.Error}
			if policyErr != nil {
				issue.Detail = policyErr.Error()
			}
		case settings.Enabled && (v.EnrollmentState == "状态已过期" || v.EnrollmentState == "后台已停止" || v.EnrollmentState == "需要关注" || v.EnrollmentState == "状态不可用" || v.EnrollmentState == "等待文件系统"):
			issue = &runtimeIssue{Source: "enrollment", Reason: "自动折叠需要关注", Impact: "新会话的压缩或回收可能需要稍后重试。",
				Recommendations: []string{"查看自动折叠日志。", "需要排查时复制或导出诊断。"}, Detail: v.EnrollmentState}
			if v.Progress != nil {
				issue.Detail += "；阶段：" + v.Progress.Phase + "；错误类型：" + v.Progress.ErrorKind
				if v.Progress.ErrorKind == "budget" {
					issue.Reason = "自动折叠的磁盘空间预算不足"
					issue.Recommendations = []string{"释放磁盘空间后等待后台重试。", "原始数据和保留副本须在校验通过后才能回收。"}
				}
			}
		}
	}
	if issue != nil && issue.Source == "enrollment" {
		v.Health, v.Healthy = "需要关注", false
	}
	if recordHistory {
		m.history.record(v, issue, now)
	}
	v.HistorySamples = append([]HistorySample(nil), m.history.archive.Samples...)
	v.Incidents = append([]Incident(nil), m.history.archive.Incidents...)
	v.HistoryError = m.history.errorText
	return v
}

func (m *Monitor) FlushHistory() { m.history.flush(time.Now()) }

func componentState(snapshot fskitstatus.Snapshot, err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "未连接"
	}
	if err != nil {
		return "状态不可用"
	}
	if text, ok := map[string]string{"healthy": "运行正常", "starting": "正在启动", "stopped": "已停止", "degraded": "需要关注", "error": "需要关注"}[snapshot.State]; ok {
		return text
	}
	return "状态未知"
}

func bytesText(value float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
