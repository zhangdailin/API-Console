package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/buildinfo"
)

const maxBinarySize = 150 * 1024 * 1024

type Operation struct {
	ID             string         `json:"id"`
	IdempotencyKey string         `json:"idempotency_key"`
	Kind           string         `json:"kind"`
	Phase          string         `json:"phase"`
	Message        string         `json:"message"`
	Target         buildinfo.Info `json:"target"`
	Previous       buildinfo.Info `json:"previous"`
	SHA256         string         `json:"sha256,omitempty"`
	Backup         string         `json:"backup,omitempty"`
	PreviousSHA256 string         `json:"previous_sha256,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func (o Operation) Busy() bool {
	switch o.Phase {
	case "queued", "downloading", "verifying", "staging", "restart_pending", "restarting", "rolling_back":
		return true
	}
	return false
}

type Backup struct {
	Path   string         `json:"path"`
	SHA256 string         `json:"sha256"`
	Build  buildinfo.Info `json:"build"`
}
type Check struct {
	Current     buildinfo.Info `json:"current"`
	Release     *Release       `json:"release,omitempty"`
	HasUpdate   bool           `json:"has_update"`
	Available   bool           `json:"available"`
	Warning     string         `json:"warning,omitempty"`
	CheckedAt   time.Time      `json:"checked_at"`
	CanUpdate   bool           `json:"can_update"`
	Reason      string         `json:"reason,omitempty"`
	CanRollback bool           `json:"can_rollback"`
	Operation   *Operation     `json:"operation,omitempty"`
}
type Manager struct {
	Info                                buildinfo.Info
	Source                              ReleaseSource
	Executable, Service, HealthURL, Dir string
	Reason                              string
	mu                                  sync.Mutex
	cached                              *Release
	checkedAt                           time.Time
	// Kept injectable to test replacement failures without restarting a machine.
	launch func(string, string, string, string, string, string) error
	rename func(string, string) error
}

func New(port string) *Manager {
	executable, _ := os.Executable()
	executable, _ = filepath.EvalSymlinks(executable)
	m := &Manager{Info: buildinfo.Current(), Source: NewGitHub(buildinfo.Repository), Executable: executable, Service: os.Getenv("ORCHIDS_UPDATE_SERVICE"), HealthURL: "http://127.0.0.1:" + port + "/health", Dir: filepath.Join(filepath.Dir(executable), ".orchids-updates"), launch: launchWatchdog, rename: os.Rename}
	m.Reason = platformCapability(executable, m.Service)
	if versionParts(m.Info.Version) == nil {
		m.Reason = "当前为无版本源码构建，请先部署带版本标识的构建"
	}
	if m.Reason == "" {
		if err := os.MkdirAll(m.Dir, 0700); err != nil {
			m.Reason = "无法创建升级状态目录"
		} else {
			// Download tasks cannot survive an unexpected application crash; no binary
			// was replaced in these stages. The armed watchdog owns restart stages.
			if op, err := m.Status(); err == nil && op.Busy() && (op.Phase == "queued" || op.Phase == "downloading" || op.Phase == "verifying" || op.Phase == "staging") {
				if op.Phase == "staging" && op.SHA256 != "" {
					if hash, e := hashFile(executable); e == nil && hash == op.SHA256 {
						op.Phase = "restart_pending"
						op.Message = "恢复已替换程序的版本验证"
						_ = m.save(&op)
						return m
					}
				}
				op.Phase = "failed"
				op.Message = "升级任务因进程退出中断，可重新检查后重试"
				_ = m.save(&op)
			}
		}
	}
	return m
}
func (m *Manager) statePath() string { return filepath.Join(m.Dir, "operation.json") }
func (m *Manager) Status() (Operation, error) {
	var op Operation
	err := readState(m.statePath(), &op)
	return op, err
}
func (m *Manager) save(op *Operation) error {
	op.UpdatedAt = time.Now().UTC()
	return writeState(m.statePath(), op)
}
func (m *Manager) backup() (Backup, error) {
	var b Backup
	e := readState(filepath.Join(m.Dir, "backup.json"), &b)
	if e == nil {
		_, e = os.Stat(b.Path)
	}
	return b, e
}
func (m *Manager) Check(ctx context.Context, force bool) Check {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Check{Current: m.Info, CanUpdate: m.Reason == "", Reason: m.Reason, CheckedAt: m.checkedAt}
	if _, err := m.backup(); err == nil {
		result.CanRollback = result.CanUpdate
	}
	if op, err := m.Status(); err == nil {
		result.Operation = &op
	}
	if force || m.cached == nil || time.Since(m.checkedAt) > 20*time.Minute {
		release, err := m.Source.Latest(ctx)
		if err != nil {
			result.Warning = err.Error()
			if m.cached == nil || time.Since(m.checkedAt) > 20*time.Minute {
				return result
			}
		} else {
			m.cached = &release
			m.checkedAt = time.Now().UTC()
		}
	}
	result.Release = m.cached
	result.Available = true
	result.CheckedAt = m.checkedAt
	if c, ok := Compare(m.cached.Tag, m.Info.Version); ok {
		result.HasUpdate = c > 0
	} else {
		result.Warning = "当前构建没有可比较的版本号"
	}
	if _, _, err := selectAssets(*m.cached, m.Info.OS, m.Info.Arch); err != nil {
		result.CanUpdate = false
		result.Reason = err.Error()
	}
	return result
}
func selectAssets(r Release, goos, arch string) (Asset, Asset, error) {
	name := "orchids-server-" + goos + "-" + arch
	var binary, checksum Asset
	metadataFound := false
	for _, a := range r.Assets {
		if a.Name == name+".build-info.txt" && a.URL != "" {
			metadataFound = true
		}
		if a.Name == name {
			binary = a
		}
		if a.Name == name+".sha256" {
			checksum = a
		}
	}
	if binary.URL == "" || checksum.URL == "" || !metadataFound {
		return binary, checksum, errors.New("发行版缺少当前平台二进制、SHA-256 或构建信息文件")
	}
	return binary, checksum, nil
}
func (m *Manager) Start(kind, tag, key string) (Operation, error) {
	if m.Reason != "" {
		return Operation{}, errors.New(m.Reason)
	}
	if len(key) < 8 || len(key) > 128 {
		return Operation{}, errors.New("需要有效的 Idempotency-Key")
	}
	if previous, err := m.Status(); err == nil && previous.IdempotencyKey == key {
		if previous.Kind != kind || (kind == "update" && previous.Target.Version != tag) {
			return Operation{}, errors.New("幂等键已用于不同操作")
		}
		return previous, nil
	}
	unlock, err := fileLock(filepath.Join(m.Dir, "operation.lock"))
	if err != nil {
		return Operation{}, err
	}
	op, err := m.Status()
	if err == nil {
		if op.IdempotencyKey == key {
			unlock()
			if op.Kind != kind || (kind == "update" && op.Target.Version != tag) {
				return Operation{}, errors.New("幂等键已用于不同操作")
			}
			return op, nil
		}
		if op.Busy() {
			unlock()
			return Operation{}, errors.New("系统升级操作正在进行")
		}
	} else if !os.IsNotExist(err) {
		unlock()
		return Operation{}, fmt.Errorf("无法读取升级状态：%w", err)
	}
	if kind != "update" && kind != "rollback" {
		unlock()
		return Operation{}, errors.New("无效操作")
	}
	if kind == "update" {
		cmp, ok := Compare(tag, m.Info.Version)
		if !ok || cmp <= 0 {
			unlock()
			return Operation{}, errors.New("目标版本必须高于当前版本")
		}
	}
	id := make([]byte, 12)
	if _, err = rand.Read(id); err != nil {
		unlock()
		return Operation{}, err
	}
	op = Operation{ID: hex.EncodeToString(id), IdempotencyKey: key, Kind: kind, Phase: "queued", Message: "准备升级任务", Previous: m.Info, Target: buildinfo.Info{Version: tag, OS: runtime.GOOS, Arch: runtime.GOARCH, BuildType: "release"}, StartedAt: time.Now().UTC()}
	if kind == "rollback" {
		b, e := m.backup()
		if e != nil {
			unlock()
			return Operation{}, errors.New("没有可回退的备份")
		}
		op.Target = b.Build
	}
	if err = m.save(&op); err != nil {
		unlock()
		return Operation{}, err
	}
	snapshot := op
	go func() {
		defer unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := m.perform(ctx, &op); err != nil {
			op.Phase = "failed"
			op.Message = err.Error()
			_ = m.save(&op)
		}
	}()
	return snapshot, nil
}
func (m *Manager) phase(op *Operation, phase, message string) error {
	op.Phase = phase
	op.Message = message
	return m.save(op)
}
func checksumDigest(text, name string) (string, error) {
	for line := range strings.SplitSeq(text, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 || strings.TrimPrefix(parts[1], "*") != name {
			continue
		}
		b, e := hex.DecodeString(parts[0])
		if e == nil && len(b) == 32 {
			return strings.ToLower(parts[0]), nil
		}
	}
	return "", errors.New("校验文件缺少目标二进制的有效 SHA-256")
}
func (m *Manager) perform(ctx context.Context, op *Operation) error {
	stage, err := os.MkdirTemp(m.Dir, "update-"+op.ID+"-")
	if err != nil {
		return err
	}
	candidate := filepath.Join(stage, "candidate")
	if op.Kind == "rollback" {
		b, e := m.backup()
		if e != nil {
			return e
		}
		hash, e := hashFile(b.Path)
		if e != nil || hash != b.SHA256 {
			return errors.New("备份校验失败，未修改当前程序")
		}
		if err = copyFile(b.Path, candidate); err != nil {
			return err
		}
		op.SHA256 = b.SHA256
	} else {
		if err = m.phase(op, "downloading", "正在下载并校验发行版"); err != nil {
			return err
		}
		r, e := m.Source.Latest(ctx)
		if e != nil {
			return e
		}
		if r.Tag != op.Target.Version {
			return errors.New("最新发行版已变化，请重新检查后升级")
		}
		binary, checksum, e := selectAssets(r, m.Info.OS, m.Info.Arch)
		if e != nil {
			return e
		}
		var sums bytes.Buffer
		if e = m.Source.Download(ctx, checksum, &sums, 64*1024); e != nil {
			return e
		}
		digest, e := checksumDigest(sums.String(), binary.Name)
		if e != nil {
			return e
		}
		f, e := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if e != nil {
			return e
		}
		e = m.Source.Download(ctx, binary, f, maxBinarySize)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if err = m.phase(op, "verifying", "正在验证 SHA-256 与平台架构"); err != nil {
			return err
		}
		hash, e := hashFile(candidate)
		if e != nil {
			return e
		}
		if hash != digest {
			return errors.New("SHA-256 不匹配，未修改当前程序")
		}
		var metadata Asset
		for _, asset := range r.Assets {
			if asset.Name == binary.Name+".build-info.txt" {
				metadata = asset
			}
		}
		if metadata.URL == "" {
			return errors.New("发行版缺少构建版本信息")
		}
		var info bytes.Buffer
		if e = m.Source.Download(ctx, metadata, &info, 64*1024); e != nil {
			return e
		}
		values := map[string]string{}
		for line := range strings.SplitSeq(info.String(), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				values[k] = strings.TrimSpace(v)
			}
		}
		if values["version"] != r.Tag || values["goos"] != m.Info.OS || values["goarch"] != m.Info.Arch || len(values["commit"]) < 7 {
			return errors.New("发行版构建信息与目标版本不一致")
		}
		op.Target.Commit = values["commit"]
		op.Target.Date = values["built_at"]
		op.Target.Repository = m.Info.Repository
		op.SHA256 = digest
	}
	if err = validateBinary(candidate); err != nil {
		return err
	}
	if err = m.phase(op, "staging", "正在备份当前程序并启动回退守护进程"); err != nil {
		return err
	}
	op.Backup = filepath.Join(stage, "previous")
	if err = copyFile(m.Executable, op.Backup); err != nil {
		return err
	}
	op.PreviousSHA256, err = hashFile(op.Backup)
	if err != nil {
		return err
	}
	if err = m.save(op); err != nil {
		return err
	}
	if err = m.launch(op.Backup, m.statePath(), m.Service, m.Executable, m.HealthURL, op.ID); err != nil {
		return err
	}
	// Keep a separate copy, then atomically rename over the existing path. There
	// is no two-rename interval in which the executable path is absent.
	if err = m.rename(candidate, m.Executable); err != nil {
		return fmt.Errorf("替换失败，当前程序保持原样：%w", err)
	}
	if err = syncDir(filepath.Dir(m.Executable)); err != nil {
		return m.restoreBeforeRestart(op, err)
	}
	if err = m.phase(op, "restart_pending", "文件已替换，等待重启和目标版本验证"); err != nil {
		return m.restoreBeforeRestart(op, err)
	}
	return nil
}
func (m *Manager) restoreBeforeRestart(op *Operation, cause error) error {
	path := op.Backup + ".restore"
	if err := copyFile(op.Backup, path); err != nil {
		return fmt.Errorf("状态持久化失败且恢复失败：%v / %w", cause, err)
	}
	if err := os.Rename(path, m.Executable); err != nil {
		return err
	}
	_ = syncDir(filepath.Dir(m.Executable))
	return cause
}

// Discovery runs independently of open browser tabs. Installation stays explicit.
func (m *Manager) StartBackground(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(20 * time.Minute)
		defer ticker.Stop()
		for {
			m.Check(ctx, false)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
