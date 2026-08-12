package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"todo2api/internal/config"
)

const (
	defaultKeyWatchInterval = 2 * time.Second
	defaultKeyWatchDebounce = 500 * time.Millisecond
)

// WatchKeyFiles polls pool.key_files and reloads the account pool when contents
// change. Transient read errors (for example mid atomic replace) are skipped so
// the last good key set is kept. logf may be nil.
func (p *Pool) WatchKeyFiles(ctx context.Context, cfg *config.Config, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	paths, err := cfg.ResolvedKeyFilePaths()
	if err != nil {
		logf("key file watch disabled: %v", err)
		return
	}
	if len(paths) == 0 {
		return
	}

	fingerprint, err := keyFilesFingerprint(paths)
	if err != nil {
		logf("key file watch initial fingerprint: %v", err)
	}

	ticker := time.NewTicker(defaultKeyWatchInterval)
	defer ticker.Stop()

	var (
		debounce *time.Timer
		debounced <-chan time.Time
	)
	stopDebounce := func() {
		if debounce == nil {
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounced:
			default:
			}
		}
		debounce = nil
		debounced = nil
	}
	defer stopDebounce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next, err := keyFilesFingerprint(paths)
			if err != nil {
				// Keep the previous set while files are temporarily unreadable.
				continue
			}
			if next == fingerprint {
				continue
			}
			fingerprint = next
			stopDebounce()
			debounce = time.NewTimer(defaultKeyWatchDebounce)
			debounced = debounce.C
		case <-debounced:
			debounce = nil
			debounced = nil
			keys, err := cfg.LoadPoolKeys()
			if err != nil {
				logf("key file reload skipped: %v", err)
				continue
			}
			stats, err := p.ReloadKeys(ctx, keys)
			if err != nil {
				logf("key file reload failed: %v (added=%d removed=%d restored=%d failed=%d ready=%d)",
					err, stats.Added, stats.Removed, stats.Restored, stats.Failed, stats.Ready)
				continue
			}
			if stats.Added == 0 && stats.Removed == 0 && stats.Restored == 0 && stats.Failed == 0 {
				continue
			}
			logf("key file reload: added=%d removed=%d restored=%d failed=%d ready=%d configured=%d",
				stats.Added, stats.Removed, stats.Restored, stats.Failed, stats.Ready, stats.Configured)
		}
	}
}

func keyFilesFingerprint(paths []string) (string, error) {
	h := sha256.New()
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
		fmt.Fprintf(h, "%s\n%d\n%d\n", path, info.Size(), info.ModTime().UnixNano())
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", path, err)
		}
		if _, err := io.Copy(h, file); err != nil {
			file.Close()
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close %s: %w", path, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
