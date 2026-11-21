// Package config reads the few process settings from the environment; everything else is set in the UI.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"podcasts/internal/engine"
)

type Config struct {
	Password  string
	MasterKey []byte
	Addr      string
	DB        string
	// Cache is the folder of the media cache.
	Cache        string
	SecureCookie bool
	Demo         bool
	// LockSettings refuses changes to Settings → Server.
	LockSettings bool
	// Engine is the downloader engine's command; empty leaves video channels off.
	Engine     string
	EngineURL  string
	EngineArgs engine.Args
	// AllowLocalNetwork lets feeds, the downloader engine and ffmpeg reach private and loopback addresses.
	AllowLocalNetwork bool
}

func Load() (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if file, ok := os.LookupEnv(key + "_FILE"); ok && file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s_FILE: %w", key, err))
				return def
			}
			return strings.TrimSpace(string(b))
		}
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Password:  get("PODCASTS_PASSWORD", ""),
		Addr:      get("PODCASTS_ADDR", ":8484"),
		DB:        get("PODCASTS_DB", "./state/podcasts.db"),
		Engine:    get("PODCASTS_ENGINE", ""),
		EngineURL: get("PODCASTS_ENGINE_URL", ""),
	}
	// An argument variable set but empty means the touchpoint has no arguments, which turns off an optional one.
	args := func(key string, def []string) []string {
		_, file := os.LookupEnv(key + "_FILE")
		_, set := os.LookupEnv(key)
		if !file && !set {
			return def
		}
		return strings.Fields(get(key, ""))
	}
	d := engine.DefaultArgs()
	c.EngineArgs = engine.Args{
		List:    args("PODCASTS_ENGINE_ARGS_LIST", d.List),
		Artwork: args("PODCASTS_ENGINE_ARGS_ARTWORK", d.Artwork),
		Video:   args("PODCASTS_ENGINE_ARGS_VIDEO", d.Video),
		Resolve: args("PODCASTS_ENGINE_ARGS_RESOLVE", d.Resolve),
		Version: args("PODCASTS_ENGINE_ARGS_VERSION", d.Version),
		Update:  args("PODCASTS_ENGINE_ARGS_UPDATE", d.Update),
	}
	if get("PODCASTS_ENGINE_LIST_ARGS", "") != "" {
		errs = append(errs, errors.New("PODCASTS_ENGINE_LIST_ARGS is gone: put those arguments in PODCASTS_ENGINE_ARGS_LIST and PODCASTS_ENGINE_ARGS_ARTWORK"))
	}
	if err := c.EngineArgs.Check(); err != nil {
		errs = append(errs, err)
	}
	c.Cache = get("PODCASTS_CACHE", filepath.Join(filepath.Dir(c.DB), "cache"))
	if c.EngineURL != "" {
		c.Engine = engineBin(c.Engine, filepath.Dir(c.DB))
	}
	if c.Password == "" {
		errs = append(errs, errors.New("PODCASTS_PASSWORD is required"))
	}
	key, err := ParseKey(get("PODCASTS_MASTER_KEY", ""))
	if err != nil {
		errs = append(errs, fmt.Errorf("PODCASTS_MASTER_KEY: %w", err))
	}
	c.MasterKey = key
	flag := func(key string) bool {
		v := get(key, "")
		if v == "" {
			return false
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		return b
	}
	c.SecureCookie = flag("PODCASTS_SECURE_COOKIE")
	c.Demo = flag("PODCASTS_DEMO")
	c.LockSettings = flag("PODCASTS_LOCK_SETTINGS")
	c.AllowLocalNetwork = flag("PODCASTS_ALLOW_LOCAL_NETWORK")
	return c, errors.Join(errs...)
}

// engineBin places a downloadable engine beside the database unless it names a path or a command already on PATH.
func engineBin(engine, dir string) string {
	if engine == "" {
		return filepath.Join(dir, "engine")
	}
	if strings.ContainsRune(engine, filepath.Separator) {
		return engine
	}
	if _, err := exec.LookPath(engine); err == nil {
		return engine
	}
	return filepath.Join(dir, engine)
}

// ParseKey accepts 32 bytes written as 64 hex digits or base64.
func ParseKey(v string) ([]byte, error) {
	if v == "" {
		return nil, errors.New("required: 32 random bytes as hex or base64 (run `podcasts password`)")
	}
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("want 32 bytes as 64 hex digits or base64")
}
