package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"orchids-api/internal/buildinfo"
)

// verifyReady validates both the actual systemd process image and the version
// reported by that process. A generic health 200 is insufficient.
func verifyReady(service, health, digest string, build buildinfo.Info, deadline time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		hash, err := runningHash(service)
		if err == nil && hash == digest {
			response, err := client.Get(health)
			if err == nil {
				var body struct {
					Status string         `json:"status"`
					Build  buildinfo.Info `json:"build"`
				}
				e := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 64*1024)).Decode(&body)
				response.Body.Close()
				if e == nil && response.StatusCode == 200 && body.Status == "ok" && body.Build.Version == build.Version && (build.Commit == "" || body.Build.Commit == build.Commit) {
					return nil
				}
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("新进程未在期限内通过目标版本、进程 SHA-256 和健康验证")
}

func Watchdog(state, service, executable, health, id string) error {
	return runWatchdog(state, service, executable, health, id, restartService, verifyReady)
}
func runWatchdog(state, service, executable, health, id string, restart func(string) error, verify func(string, string, string, buildinfo.Info, time.Duration) error) error {
	manager := &Manager{Dir: filepath.Dir(state), Executable: executable}
	var op Operation
	until := time.Now().Add(16 * time.Minute)
	for {
		if err := readState(state, &op); err != nil {
			return err
		}
		if op.ID != id {
			return nil
		}
		if op.Phase == "restart_pending" {
			break
		}
		if !op.Busy() {
			return nil
		}
		if time.Now().After(until) {
			return errors.New("等待升级准备超时")
		}
		time.Sleep(time.Second)
	}
	// Serializes watchdog completion, rollback and any later administrative action.
	var unlock func()
	var err error
	for range 10 {
		unlock, err = fileLock(filepath.Join(manager.Dir, "operation.lock"))
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	defer unlock()
	if err = readState(state, &op); err != nil {
		return err
	}
	if op.ID != id || op.Phase != "restart_pending" {
		return nil
	}
	if err = manager.phase(&op, "restarting", "正在重启并验证目标版本"); err != nil {
		return err
	}
	err = restart(service)
	if err == nil {
		err = verify(service, health, op.SHA256, op.Target, 90*time.Second)
	}
	if err == nil {
		if err = writeState(filepath.Join(manager.Dir, "backup.json"), Backup{op.Backup, op.PreviousSHA256, op.Previous}); err != nil {
			return err
		}
		return manager.phase(&op, "complete", "目标版本已启动，进程校验与健康检查通过")
	}
	cause := err
	if err = manager.phase(&op, "rolling_back", "新版本验证失败，正在恢复旧程序"); err != nil {
		return err
	}
	hash, e := hashFile(op.Backup)
	if e != nil || hash != op.PreviousSHA256 {
		return manager.phase(&op, "recovery_failed", "旧程序备份校验失败，需要人工恢复")
	}
	restore := op.Backup + ".restore"
	if err = copyFile(op.Backup, restore); err == nil {
		err = os.Rename(restore, executable)
	}
	if err == nil {
		err = syncDir(filepath.Dir(executable))
	}
	if err == nil {
		err = restart(service)
	}
	if err == nil {
		err = verify(service, health, op.PreviousSHA256, op.Previous, 90*time.Second)
	}
	if err != nil {
		return manager.phase(&op, "recovery_failed", fmt.Sprintf("恢复旧程序失败，需要人工检查：%v", err))
	}
	return manager.phase(&op, "rolled_back", fmt.Sprintf("升级未完成，旧版本已恢复并验证：%v", cause))
}

// RunWatchdogCLI is handled before loading credentials or initializing stores.
func RunWatchdogCLI(args []string) (bool, error) {
	if len(args) == 0 || args[0] != "--upgrade-watchdog" {
		return false, nil
	}
	if len(args) != 6 {
		return true, errors.New("invalid watchdog arguments")
	}
	return true, Watchdog(args[1], args[2], args[3], args[4], args[5])
}
