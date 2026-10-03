package tray

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	historyRetention = 30 * 24 * time.Hour
	historyMaxBytes  = 4 << 20
	incidentDelay    = 10 * time.Second
)

type HistorySample struct {
	T        int64    `json:"t"`
	Logical  *int64   `json:"l,omitempty"`
	Physical *int64   `json:"p,omitempty"`
	Read     *float64 `json:"r,omitempty"`
	Write    *float64 `json:"w,omitempty"`
	Health   string   `json:"health"`
}

type Incident struct {
	ID              string   `json:"id"`
	Source          string   `json:"source"`
	Reason          string   `json:"reason"`
	Impact          string   `json:"impact"`
	Recommendations []string `json:"recommendations"`
	Detail          string   `json:"detail"`
	StartedAt       int64    `json:"started_at"`
	RecoveredAt     *int64   `json:"recovered_at,omitempty"`
}

type runtimeIssue struct {
	Source, Reason, Impact, Detail string
	Recommendations                []string
}

type historyArchive struct {
	Version   int             `json:"version"`
	Samples   []HistorySample `json:"samples"`
	Incidents []Incident      `json:"incidents"`
}

type historyState struct {
	archive   historyArchive
	path      string
	errorText string
	readError bool
	dirty     bool
	lastSave  time.Time
	pending   *runtimeIssue
	since     time.Time
}

func loadHistory(store string, now time.Time) historyState {
	h := historyState{path: filepath.Join(store, "enrollment", "ui-history-v1.json"), archive: historyArchive{Version: 1}}
	file, err := openSharedFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return h
	}
	if err == nil {
		defer file.Close()
		var data []byte
		data, err = io.ReadAll(io.LimitReader(file, historyMaxBytes+1))
		if err == nil && len(data) > historyMaxBytes {
			err = errors.New("history file exceeds its size limit")
		}
		if err == nil {
			err = json.Unmarshal(data, &h.archive)
		}
		if err == nil && (h.archive.Version != 1 || len(h.archive.Samples) > 5000 || len(h.archive.Incidents) > 200) {
			err = errors.New("history version or record count is invalid")
		}
	}
	if err != nil {
		// Preserve an unreadable archive. An empty GUI history is not permission
		// to replace existing evidence with a new empty file.
		h.archive = historyArchive{Version: 1}
		h.errorText, h.readError = "历史文件无法读取，原文件已保留；新记录暂未写入。", true
		return h
	}
	h.compact(now)
	return h
}

func (h *historyState) record(view View, issue *runtimeIssue, now time.Time) {
	if view.LogicalBytes != nil || view.PhysicalBytes != nil || view.ReadRate != nil || view.WriteRate != nil {
		sample := HistorySample{T: now.UnixMilli(), Logical: view.LogicalBytes, Physical: view.PhysicalBytes,
			Read: view.ReadRate, Write: view.WriteRate, Health: view.Health}
		minute := sample.T / 60000
		count := len(h.archive.Samples)
		if count > 0 && h.archive.Samples[count-1].T/60000 == minute {
			h.archive.Samples[count-1] = sample
		} else {
			h.archive.Samples = append(h.archive.Samples, sample)
			h.dirty = true
		}
	}
	incidentChanged := h.recoverIncidents(view, now)
	before := len(h.archive.Incidents)
	h.recordIncident(issue, false, now)
	incidentChanged = incidentChanged || len(h.archive.Incidents) != before
	if h.dirty && (incidentChanged || h.lastSave.IsZero() || now.Sub(h.lastSave) >= time.Minute) {
		h.flush(now)
	}
}

func (h *historyState) recoverIncidents(view View, now time.Time) bool {
	changed := false
	for index := range h.archive.Incidents {
		item := &h.archive.Incidents[index]
		if item.RecoveredAt != nil {
			continue
		}
		healthy := item.Source == "filesystem" && view.DaemonState == "运行正常" ||
			item.Source == "managed" && view.ManagedState == "运行正常"
		if item.Source == "enrollment" {
			switch view.EnrollmentState {
			case "等待下次检查", "正在检查", "正在折叠", "正在打包", "正在迁移", "正在回收", "等待回收", "已关闭":
				healthy = view.Progress != nil && view.Progress.Fresh
			}
		}
		if healthy {
			at := now.UnixMilli()
			item.RecoveredAt, h.dirty, changed = &at, true, true
		}
	}
	return changed
}

func (h *historyState) recordIncident(issue *runtimeIssue, healthy bool, now time.Time) {
	if issue == nil {
		h.pending, h.since = nil, time.Time{}
		if healthy {
			for index := range h.archive.Incidents {
				if h.archive.Incidents[index].RecoveredAt == nil {
					at := now.UnixMilli()
					h.archive.Incidents[index].RecoveredAt = &at
					h.dirty = true
				}
			}
		}
		return
	}
	if h.pending == nil || h.pending.Source != issue.Source {
		copy := *issue
		h.pending, h.since = &copy, now
	}
	var active *Incident
	for index := range h.archive.Incidents {
		item := &h.archive.Incidents[index]
		if item.RecoveredAt == nil && item.Source == issue.Source {
			active = item
			break
		}
	}
	if active == nil && now.Sub(h.since) >= incidentDelay {
		h.archive.Incidents = append(h.archive.Incidents, Incident{ID: fmt.Sprintf("%s-%d", issue.Source, h.since.UnixNano()),
			Source: issue.Source, StartedAt: h.since.UnixMilli()})
		active = &h.archive.Incidents[len(h.archive.Incidents)-1]
		h.dirty = true
	}
	if active != nil {
		if active.Reason != issue.Reason || active.Detail != issue.Detail {
			h.dirty = true
		}
		active.Reason, active.Impact, active.Detail = issue.Reason, issue.Impact, issue.Detail
		active.Recommendations = append([]string(nil), issue.Recommendations...)
	}
}

func historyResolution(age time.Duration) time.Duration {
	if age <= 6*time.Hour {
		return time.Minute
	}
	if age <= 7*24*time.Hour {
		return 15 * time.Minute
	}
	return time.Hour
}

func (h *historyState) compact(now time.Time) {
	cutoff, future := now.Add(-historyRetention).UnixMilli(), now.Add(time.Minute).UnixMilli()
	buckets := make(map[[2]int64]HistorySample)
	for _, sample := range h.archive.Samples {
		if sample.T < cutoff || sample.T > future || sample.T <= 0 {
			continue
		}
		resolution := historyResolution(now.Sub(time.UnixMilli(sample.T))).Milliseconds()
		key := [2]int64{resolution, sample.T / resolution}
		if previous, exists := buckets[key]; !exists || sample.T > previous.T {
			buckets[key] = sample
		}
	}
	h.archive.Samples = make([]HistorySample, 0, len(buckets))
	for _, sample := range buckets {
		h.archive.Samples = append(h.archive.Samples, sample)
	}
	sort.Slice(h.archive.Samples, func(i, j int) bool { return h.archive.Samples[i].T < h.archive.Samples[j].T })
	incidents := h.archive.Incidents[:0]
	for _, item := range h.archive.Incidents {
		last := item.StartedAt
		if item.RecoveredAt != nil {
			last = *item.RecoveredAt
		}
		if last >= cutoff && item.StartedAt <= future {
			incidents = append(incidents, item)
		}
	}
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].StartedAt > incidents[j].StartedAt })
	if len(incidents) > 200 {
		incidents = incidents[:200]
	}
	h.archive.Incidents = incidents
}

func (h *historyState) flush(now time.Time) {
	if h.readError {
		return
	}
	h.compact(now)
	h.lastSave = now
	if err := writeJSONFile(h.path, h.archive); err != nil {
		h.errorText = "历史记录未保存，请检查目录权限；系统将稍后重试。"
		return
	}
	h.dirty, h.errorText = false, ""
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > historyMaxBytes {
		return errors.New("diagnostic/history payload exceeds its size limit")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".codexfold-ui-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceUIFile(temporary, path)
}
