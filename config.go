package historyhub

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the new-api.exe launch behaviour from 02.md E01-E05:
// a process-wide Asia/Shanghai timezone, an outbound proxy default, and the
// --ipPortMain / --ipPortHyb / --proxy flags. It all runs from this package's
// init() (which fires before main, because main.go imports us), so proxy/TZ
// take effect before the main relay HTTP client is built, and exe-relative data
// directories are independent of the caller's PWD.

var (
	flagIPPortMain      *string
	flagIPPortHyb       *string
	flagMaxConnsPerHost *int
	flagAModelTTLSecs   *int
	flagDBMaxOpen       *int
)

func init() {
	// E03: pin the process timezone so per-day log file boundaries (yyMMdd) and
	// all recorded timestamps are CST regardless of host TZ. The FixedZone
	// fallback needs no tzdata on disk.
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		time.Local = loc
	} else {
		time.Local = time.FixedZone("CST", 8*3600)
	}
	_ = os.Setenv("TZ", "Asia/Shanghai")

	// E04: apply the proxy from env/default now. The --proxy flag, registered
	// below, overrides at flag.Parse time — which still runs before the main
	// relay HTTP client is built (common.InitEnv precedes service.InitHttpClient).
	applyProxy(resolveProxyFromEnv())

	// Register our flags on the shared CommandLine so common.InitEnv's
	// flag.Parse() recognises them (otherwise the program exits with
	// "flag provided but not defined"). --proxy fires its side-effect during
	// Parse; the two ip:port flags are read later (post-Parse) at server-build.
	flag.Var(proxyFlag{}, "proxy", "outbound http(s) proxy, e.g. http://172.17.0.1:57890 (default); use 'none' to disable")
	flagIPPortMain = flag.String("ipPortMain", "", "main server ip:port (default 127.0.0.1:<port>)")
	flagIPPortHyb = flag.String("ipPortHyb", "", "historyhub server ip:port (default 0.0.0.0:3001)")
	// 0 表示"用户没传"，读取点回退 env 再回退内置默认（见各 *Value 函数）。
	flagMaxConnsPerHost = flag.Int("maxConnsPerHost", 0, "max active conns to main server, 0 = unlimited (env maxConnsPerHost; default 0)")
	flagAModelTTLSecs = flag.Int("aModelTTLSeconds", 0, "channel/model label cache TTL in seconds (env aModelTTLSeconds; default 300)")
	flagDBMaxOpen = flag.Int("dbMaxOpen", 0, "max cached per-user sqlite conns (env dbMaxOpen; default 1000)")
}

// ---- working directory & data dirs (E01/E02) -------------------------------

// exeDir is the directory of the running binary. All on-disk data defaults to
// live under it so the process is independent of the caller's PWD (E01).
func exeDir() string {
	if ex, err := os.Executable(); err == nil && ex != "" {
		if resolved, err := filepath.EvalSymlinks(ex); err == nil && resolved != "" {
			return filepath.Dir(resolved)
		}
		return filepath.Dir(ex)
	}
	return ""
}

func joinExe(sub string) string {
	if d := exeDir(); d != "" {
		return filepath.Join(d, sub)
	}
	return sub
}

// dbDirOverride lets tests redirect the per-user databases away from the
// exe-relative default into a temp dir (see 0/search_test.go).
var dbDirOverride string

// dbDir holds the per-user and shared sqlite files (工作目录/hybdb per E02).
func dbDir() string {
	if dbDirOverride != "" {
		return dbDirOverride
	}
	return joinExe("hybdb")
}

// fileDir holds saved attachments and system-prompt files. Fixed at
// <exe>/hybfil (N202/N203): 附件 <md5>.<ext> 与 系统提示词 <md5>.sysp.
func fileDir() string {
	return joinExe("hybfil")
}

// logDir holds the per-day request/response logs. Fixed at <exe>/hyblog
// (N204), kept separate from fileDir so attachments and logs never mix.
func logDir() string {
	return joinExe("hyblog")
}

// ---- outbound proxy (E04) --------------------------------------------------

type proxyFlag struct{}

func (proxyFlag) String() string     { return "" }
func (proxyFlag) Set(v string) error { applyProxy(v); return nil }

func resolveProxyFromEnv() string {
	// 02.md E04 names the env var "proxy" (lowercase); accept PROXY too.
	for _, k := range []string{"proxy", "PROXY"} {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "http://172.17.0.1:57890"
}

// applyProxy exports the chosen proxy to the four standard env vars consumed by
// http.ProxyFromEnvironment — used both by our reverse-proxy transport and by
// the main relay client (when no project-level proxy is configured). The
// sentinels "none"/"direct"/"off" (and empty) clear them. localhost is always
// merged into NO_PROXY so the :3001 -> :3000 hop never goes through the proxy.
func applyProxy(v string) {
	v = strings.TrimSpace(v)
	off := v == "" ||
		strings.EqualFold(v, "none") ||
		strings.EqualFold(v, "direct") ||
		strings.EqualFold(v, "off")
	keys := []string{"http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY"}
	if off {
		for _, k := range keys {
			_ = os.Unsetenv(k)
		}
		return
	}
	for _, k := range keys {
		_ = os.Setenv(k, v)
	}
	setNoProxy()
}

func setNoProxy() {
	base := []string{"127.0.0.1", "localhost", "::1"}
	merge := func(cur string) {
		for _, p := range strings.Split(cur, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			seen := false
			for _, b := range base {
				if strings.EqualFold(b, p) {
					seen = true
					break
				}
			}
			if !seen {
				base = append(base, p)
			}
		}
	}
	merge(os.Getenv("NO_PROXY"))
	merge(os.Getenv("no_proxy"))
	val := strings.Join(base, ",")
	_ = os.Setenv("NO_PROXY", val)
	_ = os.Setenv("no_proxy", val)
}

// ---- main / historyhub listen addresses (E05) ------------------------------

// MainAddr resolves the main server's listen address from --ipPortMain / env
// ipPortMain, falling back to 127.0.0.1:<port> (E05 default). Called from
// main.go at server-build time, i.e. after common.InitEnv's flag.Parse, so
// *flagIPPortMain is already populated.
func MainAddr(port string) string {
	if a := ipPortMainValue(); a != "" {
		return a
	}
	return "127.0.0.1:" + port
}

func ipPortMainValue() string {
	if flagIPPortMain != nil && *flagIPPortMain != "" {
		return *flagIPPortMain
	}
	return os.Getenv("ipPortMain")
}

// hybAddr resolves the historyhub server's listen address from --ipPortHyb /
// env ipPortHyb, defaulting to 0.0.0.0:3001 (E05). HISTORY_PORT is NOT honoured
// — 02.md E03 voids it. Read at start() time, which is after flag.Parse (start()
// waits for model.DB, set later than InitEnv).
func hybAddr() string {
	if flagIPPortHyb != nil && *flagIPPortHyb != "" {
		return *flagIPPortHyb
	}
	if v := os.Getenv("ipPortHyb"); v != "" {
		return v
	}
	return "0.0.0.0:3001"
}
