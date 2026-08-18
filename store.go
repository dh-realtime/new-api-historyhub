package historyhub

import (
	"bytes"
	"container/list"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Sen is one chat session belonging to a single new-api user. Mirrors the
// target schema in 02.md (N302): the old `model` column is gone, and `isKey`
// distinguishes web Q&A (0) from a direct API-key request (1).
//
// updated_at carries NO index (02.md N302 "精简" decision): each user's sen
// table stays small, so the listSen ORDER BY updated_at needs no index, and
// dropping it removes write overhead on every touchSen.
type Sen struct {
	Id        int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	TokenId   int    `json:"-"`
	IsKey     int    `gorm:"column:isKey;default:0" json:"isKey"`
	Title     string `gorm:"type:varchar(255)" json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Msg is one request/response turn within a Sen. It is the q/a-pair layout
// from 02.md (N303), NOT the old role/content rows. Each turn = one row.
//
// q_at / a_at are MILLISECOND timestamps (user decision: dur shown to 0.1s,
// e.g. dur:5.3s; a_at - q_at then has 100ms resolution). Legacy rows written
// before this switch hold seconds — readers normalize (>1e12 ⇒ ms).
//
// tokens_in / tokens_out carry the upstream usage counts (prompt_tokens /
// completion_tokens) when present.
//
// sen_id carries NO index (02.md N302/N303 "精简" decision): every access is
// already scoped to one user's DB, and a user's msg rows are few enough that a
// WHERE sen_id=? scan needs no index — dropping it removes write overhead on
// every insert. The legacy idx_msg_sen_id is DROPped on open if present.
type Msg struct {
	Id        int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	SenId     int64  `json:"sen_id"`
	TokensIn  int64  `gorm:"column:tokens_in;default:0" json:"tokens_in"`
	TokensOut int64  `gorm:"column:tokens_out;default:0" json:"tokens_out"`
	TTFT      int64  `gorm:"column:ttft;default:0" json:"ttft"` // 首token延迟 ms(0=未知,如非流式)
	TPS       int64  `gorm:"column:tps;default:0" json:"tps"`   // 输出阶段每秒token数(0=未知)
	QSyspId   int    `json:"q_sysp_id"`
	Q         string `gorm:"type:text" json:"q"`
	QFil      string `gorm:"type:varchar(16)" json:"q_fil"`
	QAt       int64  `json:"q_at"`
	AAt       int64  `json:"a_at"`
	AModel    string `gorm:"type:varchar(16)" json:"a_model"`
	A         string `gorm:"type:text" json:"a"`
	AFil      string `gorm:"type:varchar(16)" json:"a_fil"`
}

// HttpSystemPrompt dedups the (often huge, repeating) system prompts sent by
// agents like OpenClaw. The table holds only the prompt's md5 (a fast, compact
// unique key for de-dup); the full text is NOT stored in the DB — it is saved
// as a file hybfil/<md5>.sysp. So there is no large TEXT column at all (N301).
type HttpSystemPrompt struct {
	Id      int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	Tag     string `gorm:"type:varchar(16)" json:"tag"`
	SyspMd5 string `gorm:"column:sysp_md5;type:text;uniqueIndex:idx_httpSystemPrompt_sysp_md5" json:"sysp_md5"`
}

// HttpFile registers one file physically stored under hybfil/. fPath is the
// on-disk name (md5 + extension). The unique index on fPath gives us cheap
// dedup so identical uploads are stored once (N301).
type HttpFile struct {
	Id    int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	FLen  int64  `gorm:"column:fLen" json:"fLen"`
	FNam  string `gorm:"column:fNam" json:"fNam"`
	FPath string `gorm:"column:fPath;type:varchar(40);uniqueIndex:idx_httpFile_fPath" json:"fPath"`
}

func (Sen) TableName() string              { return "sen" }
func (Msg) TableName() string              { return "msg" }
func (HttpSystemPrompt) TableName() string { return "httpSystemPrompt" }
func (HttpFile) TableName() string         { return "httpFile" }

func now() int64 { return time.Now().Unix() }

// nowMS is the millisecond counterpart of now(), for msg.q_at / msg.a_at.
func nowMS() int64 { return time.Now().UnixMilli() }

// ---------------------------------------------------------------------------
// Per-user SQLite sharding (N03/N04/N05).
//
// Each user's history lives in its own file dbDir()/u<userId>.db
// (e.g. u000000121.db; dbDir defaults to <exe>/hybdb). Sharding by user removes
// cross-user write contention (each SQLite file has a single writer) and lets
// different users' reads /
// writes proceed in parallel. The new-api schema is never touched.
//
// Connections are cached LRU (dbMaxOpen, default 1000) so a large
// user base does not exhaust file descriptors.
// ---------------------------------------------------------------------------

type cacheEntry struct {
	userId int
	gdb    *gorm.DB
}

type dbCache struct {
	mu    sync.Mutex
	items map[int]*list.Element
	lru   *list.List
}

var shard = &dbCache{
	items: make(map[int]*list.Element),
	lru:   list.New(),
}

func dbMaxOpen() int {
	if flagDBMaxOpen != nil && *flagDBMaxOpen > 0 {
		return *flagDBMaxOpen
	}
	if n, err := strconv.Atoi(os.Getenv("dbMaxOpen")); err == nil && n > 0 {
		return n
	}
	return 1000
}

func userDBPath(userId int) string {
	return filepath.Join(dbDir(), fmt.Sprintf("u%09d.db", userId))
}

// ensureUserSchema drops the legacy sen/msg tables if this user DB still has
// the old (role/content) layout, so AutoMigrate can recreate them with the new
// q/a-pair schema. Decision: "删表重建" — old test data is discarded.
func ensureUserSchema(d *gorm.DB) {
	var info []struct {
		Name string `gorm:"column:name"`
	}
	d.Raw("PRAGMA table_info(msg)").Scan(&info)
	hasQSyspId := false
	for _, c := range info {
		if c.Name == "q_sysp_id" {
			hasQSyspId = true
			break
		}
	}
	if hasQSyspId {
		return
	}
	// Either a fresh DB (no msg table) or the legacy schema — (re)create clean.
	d.Exec("DROP TABLE IF EXISTS msg")
	d.Exec("DROP TABLE IF EXISTS sen")
}

func openUserDB(userId int) (*gorm.DB, error) {
	dir := dbDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// DSN style mirrors new-api's own SQLite usage (glebarez/sqlite).
	dsn := userDBPath(userId) + "?_busy_timeout=30000&_journal_mode=WAL"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if sqlDB, e := gdb.DB(); e == nil {
		// SQLite has a single writer per file; one connection per user DB is
		// the safe choice (no "database is locked" under a single user's burst).
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}
	ensureUserSchema(gdb)
	if err := gdb.AutoMigrate(&Sen{}, &Msg{}); err != nil {
		common.SysError("historyhub: migrate failed for user " + strconv.Itoa(userId) + ": " + err.Error())
	}
	// N302/N303 "精简": drop the now-retired indexes if a previous build created
	// them. Idempotent — a no-op when they are already gone.
	gdb.Exec("DROP INDEX IF EXISTS idx_sen_updated_at")
	gdb.Exec("DROP INDEX IF EXISTS idx_msg_sen_id")
	return gdb, nil
}

// get returns the user's *gorm.DB, opening and caching it. A cold open happens
// outside the cache lock so it never blocks other users' cached lookups.
func (c *dbCache) get(userId int) (*gorm.DB, error) {
	if userId <= 0 {
		return nil, errors.New("invalid user id")
	}
	c.mu.Lock()
	if el, ok := c.items[userId]; ok {
		c.lru.MoveToFront(el)
		gdb := el.Value.(*cacheEntry).gdb
		c.mu.Unlock()
		return gdb, nil
	}
	c.mu.Unlock()

	gdb, err := openUserDB(userId)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Race: another goroutine opened this user first — keep theirs, close ours.
	if el, ok := c.items[userId]; ok {
		existing := el.Value.(*cacheEntry).gdb
		c.lru.MoveToFront(el)
		if sqlDB, e := gdb.DB(); e == nil {
			sqlDB.Close()
		}
		return existing, nil
	}
	c.items[userId] = c.lru.PushFront(&cacheEntry{userId: userId, gdb: gdb})
	c.evictLocked()
	return gdb, nil
}

func (c *dbCache) evictLocked() {
	maxOpen := dbMaxOpen() // 在使用点解析（晚于 flag.Parse），不能在 var 初始化时固化
	for c.lru.Len() > maxOpen {
		back := c.lru.Back()
		if back == nil {
			return
		}
		entry := c.lru.Remove(back).(*cacheEntry)
		delete(c.items, entry.userId)
		if sqlDB, e := entry.gdb.DB(); e == nil {
			sqlDB.Close()
		}
	}
}

func userTx(userId int, fn func(d *gorm.DB)) {
	d, err := shard.get(userId)
	if err != nil {
		common.SysError("historyhub: open user db failed: " + err.Error())
		return
	}
	fn(d)
}

// ---------------------------------------------------------------------------
// Shared databases (httpSystemPrompt.db / httpFile.db) live alongside the
// per-user files in dbDir() (hybdb). They are process-global, opened once.
// ---------------------------------------------------------------------------

var (
	syspDBOnce sync.Once
	syspDB     *gorm.DB
	fileDBOnce sync.Once
	fileDB     *gorm.DB
)

func sharedDB(name string, once *sync.Once, ptr **gorm.DB, migrate func(*gorm.DB) error) *gorm.DB {
	once.Do(func() {
		if err := os.MkdirAll(dbDir(), 0o755); err != nil {
			common.SysError("historyhub: mkdir shared db dir failed: " + err.Error())
			return
		}
		dsn := filepath.Join(dbDir(), name) + "?_busy_timeout=30000&_journal_mode=WAL"
		gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			common.SysError("historyhub: open " + name + " failed: " + err.Error())
			return
		}
		if sqlDB, e := gdb.DB(); e == nil {
			sqlDB.SetMaxOpenConns(1)
			sqlDB.SetMaxIdleConns(1)
		}
		if err := migrate(gdb); err != nil {
			common.SysError("historyhub: migrate " + name + " failed: " + err.Error())
		}
		*ptr = gdb
	})
	return *ptr
}

// migrateSyspDB rebuilds httpSystemPrompt.db to the md5-only schema. The table
// keeps just (id, tag, sysp_md5); the prompt text lives as a file. If the
// existing table lacks the sysp_md5 column (old layout, or the previous
// sysp_hash layout) it is dropped and recreated. Existing rows are discarded —
// prompts are re-inserted (de-duped) as requests arrive.
func migrateSyspDB(d *gorm.DB) error {
	var info []struct {
		Name string `gorm:"column:name"`
	}
	d.Raw("PRAGMA table_info(httpSystemPrompt)").Scan(&info)
	hasMd5 := false
	for _, c := range info {
		if c.Name == "sysp_md5" {
			hasMd5 = true
		}
	}
	if !hasMd5 {
		d.Exec("DROP TABLE IF EXISTS httpSystemPrompt")
	}
	return d.AutoMigrate(&HttpSystemPrompt{})
}

func migrateFileDB(d *gorm.DB) error {
	// The pre-built httpFile.db already matches HttpFile exactly (incl. the
	// unique index on fPath). AutoMigrate is a no-op there; for a fresh DB it
	// creates the table.
	return d.AutoMigrate(&HttpFile{})
}

func syspdb() *gorm.DB { return sharedDB("httpSystemPrompt.db", &syspDBOnce, &syspDB, migrateSyspDB) }
func filedb() *gorm.DB { return sharedDB("httpFile.db", &fileDBOnce, &fileDB, migrateFileDB) }

// dedupSysp returns the id (and md5 file stem) of an existing identical system
// prompt, or records a new one: it writes the prompt text to hybfil/<md5>.sysp
// and inserts a row holding only the md5. tag is a short label (e.g. an agent
// name from User-Agent). The full text is never stored in the table. The md5
// lets the N204 log reference the file instead of inlining the prompt.
func dedupSysp(tag, sysp string) (int, string) {
	if strings.TrimSpace(sysp) == "" {
		return 0, ""
	}
	d := syspdb()
	if d == nil {
		return 0, ""
	}
	sum := md5.Sum([]byte(sysp))
	md5hex := hex.EncodeToString(sum[:])

	var sp HttpSystemPrompt
	if err := d.Select("id").Where("sysp_md5 = ?", md5hex).First(&sp).Error; err == nil {
		return int(sp.Id), md5hex // already recorded — file already on disk
	}

	// Persist the prompt text to hybfil/<md5>.sysp, then insert. The unique
	// index on sysp_md5 guards races; on conflict we re-read.
	if err := os.MkdirAll(fileDir(), 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(fileDir(), md5hex+".sysp"), []byte(sysp), 0o644)
	}
	sp = HttpSystemPrompt{Tag: truncate(tag, 16), SyspMd5: md5hex}
	if err := d.Create(&sp).Error; err != nil {
		if e2 := d.Select("id").Where("sysp_md5 = ?", md5hex).First(&sp).Error; e2 == nil {
			return int(sp.Id), md5hex
		}
		return 0, ""
	}
	return int(sp.Id), md5hex
}

// saveFile writes data to hybfil/<md5><ext>, registers it in httpFile.db
// (de-duped by fPath) and returns the httpFile id. Errors are logged and
// reported; callers tolerate 0 (no attachment recorded).
func saveFile(fNam string, data []byte) int64 {
	d := filedb()
	if d == nil || len(data) == 0 {
		return 0
	}
	sum := md5.Sum(data)
	fPath := hex.EncodeToString(sum[:]) + extOf(fNam)

	// Reuse an existing entry (identical content already stored).
	var hf HttpFile
	if err := d.Select("id").Where("fPath = ?", fPath).First(&hf).Error; err == nil {
		return hf.Id
	}

	// Persist the bytes, then insert. If another goroutine raced ahead on the
	// same fPath, the unique index makes our insert fail and we re-read.
	if err := os.MkdirAll(fileDir(), 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(fileDir(), fPath), data, 0o644)
	}
	hf = HttpFile{FLen: int64(len(data)), FNam: truncate(fNam, 255), FPath: fPath}
	if err := d.Create(&hf).Error; err != nil {
		if e2 := d.Select("id").Where("fPath = ?", fPath).First(&hf).Error; e2 == nil {
			return hf.Id
		}
		return 0
	}
	return hf.Id
}

// httpFileById looks up a registered file (used when serving attachments).
func httpFileById(id int64) (*HttpFile, bool) {
	d := filedb()
	if d == nil {
		return nil, false
	}
	var hf HttpFile
	if err := d.Where("id = ?", id).First(&hf).Error; err != nil {
		return nil, false
	}
	return &hf, true
}

// ---------------------------------------------------------------------------
// Username cache (for per-day log file naming). new-api usernames rarely
// change, so we cache to avoid a main-DB hit on every request.
// ---------------------------------------------------------------------------

var userNameCache sync.Map // map[int]string

// userLabelOf returns the per-user log label "用户名+显示名称" (02.md N204). Both
// come from one GetUserById and are cached: usernames rarely change.
func userLabelOf(userId int) string {
	if v, ok := userNameCache.Load(userId); ok {
		return v.(string)
	}
	label := fmt.Sprintf("u%d", userId)
	if u, err := model.GetUserById(userId, false); err == nil && u != nil {
		name := strings.TrimSpace(u.Username)
		disp := strings.TrimSpace(u.DisplayName)
		if name == "" {
			name = label
		}
		if disp != "" && disp != name {
			label = name + disp
		} else {
			label = name
		}
	}
	userNameCache.Store(userId, label)
	return label
}

// ---------------------------------------------------------------------------
// Per-day request/response log: <logDir>/<用户名+显示名称>-<yyMMdd>.log (N204).
// One handle per (user, day); each handle has its own mutex so different users'
// logs write concurrently. Each completed request writes a full block: request
// headers + body, response headers + body, token usage, datetime, duration.
// ---------------------------------------------------------------------------

type logHandle struct {
	mu sync.Mutex
	f  *os.File
}

var (
	logHandlesMu sync.Mutex
	logHandles   = map[string]*logHandle{}
)

func writeLog(userId int, content string) {
	name := sanitizeName(userLabelOf(userId))
	key := name + "." + time.Now().Format("060102")

	logHandlesMu.Lock()
	h, ok := logHandles[key]
	if !ok {
		_ = os.MkdirAll(logDir(), 0o755)
		f, err := os.OpenFile(filepath.Join(logDir(), key+".log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			logHandlesMu.Unlock()
			return
		}
		h = &logHandle{f: f}
		logHandles[key] = h
	}
	logHandlesMu.Unlock()

	h.mu.Lock()
	_, _ = h.f.WriteString(content)
	h.mu.Unlock()
}

// logLine writes a single timestamped line (used for short status notes).
func logLine(userId int, text string) {
	writeLog(userId, time.Now().Format("2006-01-02 15:04:05")+" "+text+"\n")
}

// httpLogEntry is everything captured about one proxied request/response, used
// to write the N204 log block. respHeaders/respBody may be empty for an
// upstream-unreachable failure (see note).
type httpLogEntry struct {
	userId        int
	method        string
	path          string
	status        int
	durationMS    int64
	reqHeaders    http.Header
	reqBody       []byte
	syspFile      string // hybfil/<md5>.sysp reference for the system prompt ("" if none)
	respHeaders   http.Header
	respBody      []byte
	promptTok     int
	completionTok int
	note          string // e.g. "upstream unreachable: ..."
}

// logHTTP writes the request/response block for a turn (N204). Called once
// after the response has been sent back to the user. The response body is run
// through mergeSSELines: for an SSE stream, consecutive data: chunks differing
// in one string field are merged (value concatenated); blank lines are dropped.
// data:<mime>;base64,<payload> occurrences are rewritten to the saved file name
// <md5><ext> — ONLY in what is written to the log (maskDataURLsForLog); the
// bytes actually sent to the upstream / streamed back to the client are never
// modified.
func logHTTP(e httpLogEntry) {
	var sb strings.Builder
	sb.WriteString("====================  ")
	sb.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "  %s %s  status=%d  dur=%dms", e.method, e.path, e.status, e.durationMS)
	if e.promptTok > 0 || e.completionTok > 0 {
		fmt.Fprintf(&sb, "  tokens=in:%d/out:%d", e.promptTok, e.completionTok)
	}
	if e.note != "" {
		sb.WriteString("  ")
		sb.WriteString(e.note)
	}
	sb.WriteByte('\n')

	// request: full headers (API key in plaintext per N204) + full body.
	sb.WriteString(headerDump(e.reqHeaders))
	reqLog := e.reqBody
	if e.syspFile != "" {
		reqLog = maskSystemPromptForLog(reqLog, e.syspFile)
	}
	reqLog = maskDataURLsForLog(reqLog)
	sb.WriteString(bodyForLog(string(reqLog)))
	sb.WriteByte('\n')

	// response: full headers + body (SSE data: lines merged, no blank lines).
	sb.WriteString("  响应字节数: ")
	sb.WriteString(strconv.Itoa(len(e.respBody)))
	sb.WriteString(headerDump(e.respHeaders))
	sb.WriteString(bodyForLog(string(maskDataURLsForLog([]byte(mergeSSELines(e.respBody))))))
	sb.WriteByte('\n')

	writeLog(e.userId, sb.String())
}

// maskSystemPromptForLog replaces the content of every system message in the
// request body with the sysp file name — hybfil/<md5>.sysp already holds the
// full text (dedupSysp), so the log stays small (42). Order-preserving via
// ordJSON: everything except the system content renders as-is. Applies ONLY to
// the log text — forwarded bytes are untouched.
func maskSystemPromptForLog(body []byte, syspFile string) []byte {
	if !bytes.Contains(body, []byte(`"system"`)) {
		return body
	}
	root, err := parseOrd(body)
	if err != nil || root.keys == nil {
		return body
	}
	msgs := root.vals["messages"]
	if msgs == nil || msgs.arr == nil {
		return body
	}
	// md5 hex + ".sysp" needs no JSON escaping
	repl := &ordJSON{scalar: []byte(`"` + syspFile + `"`)}
	changed := false
	for _, m := range msgs.arr {
		if m.keys == nil {
			continue
		}
		role := m.vals["role"]
		if role == nil || role.scalar == nil || string(role.scalar) != `"system"` {
			continue
		}
		if _, ok := m.vals["content"]; ok {
			m.vals["content"] = repl
			changed = true
		}
	}
	if !changed {
		return body
	}
	var sb strings.Builder
	root.marshal(&sb)
	return []byte(sb.String())
}

// dataURLRe matches data:<mime>;base64,<payload> inside a body. The base64
// class excludes JSON-significant chars, so a payload inside a JSON string
// matches exactly.
var dataURLRe = regexp.MustCompile(`data:([a-zA-Z0-9!#$&^_.+-]+/[a-zA-Z0-9!#$&^_.+-]+);base64,([A-Za-z0-9+/=]+)`)

// maskDataURLsForLog rewrites every data:<mime>;base64,<payload> in the log
// text to <md5><ext>: the payload is decoded, saved into hybfil/ and
// registered in httpFile (md5-deduped, idempotent), and the data URL is
// replaced by that file's on-disk name. Images / audio / video / documents all
// follow the same rule (ext derived from the mime; unknown → .bin). Applies
// ONLY to log rendering — forwarded traffic is untouched.
func maskDataURLsForLog(body []byte) []byte {
	if !bytes.Contains(body, []byte(";base64,")) {
		return body
	}
	return dataURLRe.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := dataURLRe.FindSubmatch(m)
		mime := string(sub[1])
		data, err := base64.StdEncoding.DecodeString(string(sub[2]))
		if err != nil {
			if data, err = base64.RawStdEncoding.DecodeString(string(sub[2])); err != nil {
				return m // 无法解码: 保留原文
			}
		}
		if len(data) == 0 {
			return m
		}
		saveFile("upload"+mimeToExt(mime), data) // md5 去重; 失败不影响替换名
		sum := md5.Sum(data)
		return []byte(hex.EncodeToString(sum[:]) + mimeToExt(mime))
	})
}

// headerDump renders an http.Header verbatim as "Key: value\n" lines. The API
// key (Authorization) is logged in PLAINTEXT per 02.md N204.
func headerDump(h http.Header) string {
	if len(h) == 0 {
		return "(none)\n"
	}
	var sb strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			sb.WriteString("  [")
			sb.WriteString(k)
			sb.WriteByte(':')
			sb.WriteString(v)
			sb.WriteByte(']')
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

// bodyForLog returns the body text verbatim — 日志永不截断(用户明确要求);
// 空体标记为 (empty)。脱敏/替换只发生在调用侧(maskDataURLsForLog 等,
// 即"只替换日志记录的内容"边界 Z20401)。
func bodyForLog(s string) string {
	if len(s) == 0 {
		return "(empty)"
	}
	return s
}

// ---- CRUD (all scoped to the caller's user DB; cross-user access is impossible) ----

func listSen(userId int) ([]Sen, error) {
	return listSenFiltered(userId, true)
}

// listSenFiltered lists a user's sessions. The web chat frontend shows its own
// chats only (isKey=0); ?senKey=1 additionally lists sessions recorded from
// direct API-key / agent traffic (isKey=1).
func listSenFiltered(userId int, includeKey bool) ([]Sen, error) {
	var convs []Sen
	userTx(userId, func(d *gorm.DB) {
		q := d.Order("updated_at desc, id desc").Limit(500)
		if !includeKey {
			q = q.Where("isKey = 0")
		}
		q.Find(&convs)
	})
	return convs, nil
}

// SenListItem enriches a sen row with sidebar aggregates: the turn count and
// the first/last activity timestamps (from its msg rows; ms when the rows are
// new, seconds for legacy rows — readers normalize by magnitude).
type SenListItem struct {
	Id        int64  `json:"id"`
	IsKey     int    `gorm:"column:isKey" json:"isKey"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	MsgCount  int64  `gorm:"column:msg_count" json:"msg_count"`
	FirstAt   int64  `gorm:"column:first_at" json:"first_at"`
	LastAt    int64  `gorm:"column:last_at" json:"last_at"`
}

// senSearch controls the sidebar search (71/835/836): space-separated AND terms
// with plain-text grammar (no regex), q/a field scopes, case sensitivity and
// an AND-combined date-range filter. The session TITLE no longer participates
// (835 user decision) — only msg q/a text is searched. Since 836 the AND unit
// is a single turn on a single side: plain terms must co-occur in the same
// question or the same answer; 问:/答: terms pin their own side.
type senSearch struct {
	includeKey bool
	keyword    string       // 兼容字段：terms 为空时作为单个普通词生效
	terms      []searchTerm // 834/835: 空格分隔的多条件，全部满足(AND)
	scopeQ     bool         // term matches msg.q (field 0 的词按此范围)
	scopeA     bool         // term matches msg.a
	caseSense  bool         // 区分大小写(835 默认不区分)
	dateFromMS int64        // 0 = unbounded
	dateToMS   int64        // 0 = unbounded
}

// searchTerm is one parsed search condition (835 普通用户语法):
//
//		[-] [问:|答:] (带引号短语 | 裸词[|裸词...])
//
//	  - 前缀 - 表示排除：范围内任一轮命中该词即淘汰该会话
//	  - 问:/答: 把该词限定到 问题/答案 单一字段(优先于 scopeQ/scopeA chips)
//	  - 引号内空格/|/问: 等均为字面字符；裸词内 | 分隔候选，任一命中即满足
type searchTerm struct {
	alts   []string // 候选词(普通文本)；单词时长度 1
	negate bool     // - 前缀：排除
	field  byte     // 0=按 scopeQ/scopeA; 'q'=问:; 'a'=答:
}

// parseSearchTerms 把搜索串拆成多个 AND 条件(835)。空白(含全角)分隔；
// "..." 引号内保留空格为整词短语。裸 - 、空引号、问:/答: 后无内容时丢弃该条件。
func parseSearchTerms(q string) []searchTerm {
	var terms []searchTerm
	rs := []rune(q)
	i := 0
	for i < len(rs) {
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
		if i >= len(rs) {
			break
		}
		t := searchTerm{}
		if rs[i] == '-' {
			t.negate = true
			i++
		}
		if i+1 < len(rs) && rs[i] == '问' && rs[i+1] == ':' {
			t.field = 'q'
			i += 2
		} else if i+1 < len(rs) && rs[i] == '答' && rs[i+1] == ':' {
			t.field = 'a'
			i += 2
		}
		var text []rune
		quoted := false
		if i < len(rs) && rs[i] == '"' {
			quoted = true
			i++
			for i < len(rs) && rs[i] != '"' {
				text = append(text, rs[i])
				i++
			}
			if i < len(rs) {
				i++ // 收掉闭引号
			}
		} else {
			for i < len(rs) && !unicode.IsSpace(rs[i]) && rs[i] != '"' {
				text = append(text, rs[i])
				i++
			}
		}
		s := string(text)
		if s == "" {
			continue // 裸 - / 问: / ""
		}
		if quoted {
			t.alts = []string{s}
		} else {
			for _, part := range strings.Split(s, "|") {
				if part != "" {
					t.alts = append(t.alts, part)
				}
			}
			if len(t.alts) == 0 {
				continue // "音乐|" 这类空候选
			}
		}
		terms = append(terms, t)
	}
	return terms
}

// splitSearchTerms 把搜索串拆成多个条件(834 AND 语义)：按空白(含全角空格/制表)分隔；
// "..." 引号内的空白保留为同一短语(引号本身去掉)。空串/纯空白/纯引号返回 nil。
func splitSearchTerms(q string) []string {
	var terms []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, cur.String())
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			if inQuote {
				inQuote = false
				flush()
			} else {
				flush() // 引号前的半截作为独立条件
				inQuote = true
			}
		case !inQuote && unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

// listSenRich lists a user's sessions with msg aggregates, then applies the
// search filters in Go: SQL LIKE can't express case-sensitivity switches, and
// per-user data volumes are small. The TITLE is not searched (835) — only msg
// q/a text per scopeQ/scopeA (or the term's own 问:/答: field). Since 836 a
// session hits only if ONE turn satisfies all terms on one side (plain terms
// co-occur in the same question or answer; see msgQualifies). Date range ANDs
// with the terms; it matches per msg turn (83): a session hits only if one of
// its turns actually falls inside the range — a session spanning the range
// with no turn on those days does NOT match.
func listSenRich(userId int, s senSearch) ([]SenListItem, error) {
	var rows []SenListItem
	userTx(userId, func(d *gorm.DB) {
		query := `SELECT s.id, s."isKey" AS isKey, s.title, s.created_at, s.updated_at,
			COALESCE(c.cnt, 0) AS msg_count,
			COALESCE(c.first_at, 0) AS first_at,
			COALESCE(c.last_at, 0) AS last_at
			FROM sen s
			LEFT JOIN (SELECT sen_id, COUNT(*) AS cnt, MIN(q_at) AS first_at,
				MAX(CASE WHEN a_at > 0 THEN a_at ELSE q_at END) AS last_at
				FROM msg GROUP BY sen_id) c ON c.sen_id = s.id`
		if !s.includeKey {
			query += ` WHERE s."isKey" = 0`
		}
		query += ` ORDER BY s.updated_at DESC, s.id DESC LIMIT 500`
		d.Raw(query).Scan(&rows)
	})
	for i := range rows {
		// 无 msg 行的会话(如上游不可达只建了 sen)：回退用 sen 自身时间
		if rows[i].MsgCount == 0 {
			rows[i].FirstAt = rows[i].CreatedAt
			rows[i].LastAt = rows[i].UpdatedAt
		}
	}

	// 反转区间(from>to)直接无结果 —— 空窗口按"重叠"数学判会误命中跨点会话(测试矩阵 D10)。
	if s.dateFromMS > 0 && s.dateToMS > 0 && s.dateFromMS > s.dateToMS {
		return nil, nil
	}
	dateActive := s.dateFromMS > 0 || s.dateToMS > 0

	// 关键词条件(834/835/836)：terms 为空时回退把整个 keyword 当一个普通词。
	// 836 起命中的单位是「同一轮的同一边」：所有普通(field 0)正词必须同时出现在
	// 该轮的问题文本或答案文本(同一边)里；问:/答: 正词固定在自己的字段上判定。
	// -排除词按轮判定：该轮内出现(普通词任一启用边/带前缀固定边)即淘汰该轮；
	// 会话只要有任意一轮同时满足全部正词且不含排除词就命中。
	terms := s.terms
	if len(terms) == 0 && s.keyword != "" {
		terms = []searchTerm{{alts: []string{s.keyword}}}
	}
	type termMatcher struct {
		t        searchTerm
		lowerAlt []string // 与 alts 一一对应(不区分大小写时用)
	}
	matchers := make([]termMatcher, 0, len(terms))
	for _, term := range terms {
		m := termMatcher{t: term}
		for _, a := range term.alts {
			m.lowerAlt = append(m.lowerAlt, strings.ToLower(a))
		}
		matchers = append(matchers, m)
	}
	altHit := func(m termMatcher, text string) bool {
		for k, alt := range m.t.alts {
			if s.caseSense {
				if strings.Contains(text, alt) {
					return true
				}
			} else if strings.Contains(strings.ToLower(text), m.lowerAlt[k]) {
				return true
			}
		}
		return false
	}
	// msgQualifies(836): 该轮是否满足全部条件 —— 同轮且普通词凑在同一边。
	msgQualifies := func(msg Msg) bool {
		plainQ, plainA := true, true // 普通正词全部落在 问题/答案 一边的可行性
		hasPlain := false
		for _, m := range matchers {
			if m.t.negate {
				continue
			}
			switch m.t.field {
			case 'q':
				if !altHit(m, msg.Q) {
					return false
				}
			case 'a':
				if !altHit(m, msg.A) {
					return false
				}
			default:
				hasPlain = true
				if s.scopeQ && !altHit(m, msg.Q) {
					plainQ = false
				}
				if s.scopeA && !altHit(m, msg.A) {
					plainA = false
				}
			}
		}
		if hasPlain && !((s.scopeQ && plainQ) || (s.scopeA && plainA)) {
			return false // 普通词没能在同一边凑齐
		}
		for _, m := range matchers {
			if !m.t.negate {
				continue
			}
			switch m.t.field {
			case 'q':
				if altHit(m, msg.Q) {
					return false
				}
			case 'a':
				if altHit(m, msg.A) {
					return false
				}
			default:
				if (s.scopeQ && altHit(m, msg.Q)) || (s.scopeA && altHit(m, msg.A)) {
					return false
				}
			}
		}
		return true
	}

	// 关键词与日期过滤都需要 msg 行，统一分块取一次
	byID := make(map[int64][]Msg, len(rows))
	if dateActive || len(matchers) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.Id)
		}
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > 500 {
				chunk = ids[:500]
				ids = ids[500:]
			} else {
				ids = nil
			}
			var msgs []Msg
			userTx(userId, func(d *gorm.DB) {
				d.Select("id", "sen_id", "q_at", "a_at", "q", "a").Where("sen_id IN ?", chunk).Order("id asc").Find(&msgs)
			})
			for _, m := range msgs {
				byID[m.SenId] = append(byID[m.SenId], m)
			}
		}
	}

	// 日期范围过滤(与关键词 AND，83)：逐条 msg 判定 —— 会话在范围内真的发生过
	// 至少一轮对话(该轮的提问或回答落在区间内)才算命中。此前按活动窗口
	// [first,last] 与区间"有交集"判定，13/15 两天有对话、中间 14 号一轮都没有的
	// 会话也会混进"仅14号"的结果。无 msg 的会话退回用 sen 自身时间窗(建立日)。
	if dateActive {
		inWin := func(t int64) bool {
			return (s.dateFromMS == 0 || t >= s.dateFromMS) && (s.dateToMS == 0 || t <= s.dateToMS)
		}
		kept := rows[:0]
		for _, r := range rows {
			if r.MsgCount == 0 {
				first, last := toMS(r.FirstAt), toMS(r.LastAt)
				if (s.dateFromMS > 0 && last < s.dateFromMS) || (s.dateToMS > 0 && first > s.dateToMS) {
					continue
				}
				kept = append(kept, r)
				continue
			}
			for _, m := range byID[r.Id] {
				if (m.QAt > 0 && inWin(toMS(m.QAt))) || (m.AAt > 0 && inWin(toMS(m.AAt))) {
					kept = append(kept, r)
					break
				}
			}
		}
		rows = kept
	}

	if len(matchers) == 0 {
		return rows, nil
	}

	// 835: 标题退出匹配，条件只对 msg 的问题/答案判定；836: 命中单位=单轮同侧 ——
	// 会话只要有任意一轮 msgQualifies 就保留(无 msg 的会话不会被任何关键词命中)。
	kept := make([]SenListItem, 0, len(rows))
	for _, r := range rows {
		for _, msg := range byID[r.Id] {
			if msgQualifies(msg) {
				kept = append(kept, r)
				break
			}
		}
	}
	return kept, nil
}

// toMS normalizes a timestamp that may be seconds (legacy) or milliseconds.
func toMS(ts int64) int64 {
	if ts > 0 && ts < 1e12 {
		return ts * 1000
	}
	return ts
}

func createSen(userId, tokenId, isKey int, title string) (*Sen, error) {
	d, err := shard.get(userId)
	if err != nil {
		return nil, err
	}
	conv := &Sen{
		TokenId:   tokenId,
		IsKey:     isKey,
		Title:     title,
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	if err := d.Create(conv).Error; err != nil {
		return nil, err
	}
	return conv, nil
}

// insertMsgQ is phase 1 of a turn (N106): record the request side right after
// the request has been sent to the upstream. The response side is filled in
// later by updateMsgA. qAt is a millisecond timestamp.
func insertMsgQ(userId int, senId int64, qSyspId int, q, qFil string, qAtMS int64) int64 {
	if senId == 0 {
		return 0
	}
	d, err := shard.get(userId)
	if err != nil {
		return 0
	}
	m := &Msg{
		SenId:   senId,
		QSyspId: qSyspId,
		Q:       q,
		QFil:    qFil,
		QAt:     qAtMS,
	}
	if err := d.Create(m).Error; err != nil {
		return 0
	}
	return m.Id
}

// updateMsgA is phase 2 of a turn (N106): fill in the response side after the
// response has been returned to the user — answer, model label, response
// attachments, token usage, TTFT/TPS and the millisecond a_at. Also bumps the
// session's updated_at.
//
// ttftMS is the first-token latency (0 for non-stream / unknown); see
// computeTPS for the tps口径 (output phase, burst fallback to whole duration).
func updateMsgA(userId int, msgId int64, aModel, a, aFil string, tokIn, tokOut int, ttftMS, durMS int64) {
	if msgId == 0 {
		return
	}
	tps := computeTPS(tokOut, ttftMS, durMS)
	userTx(userId, func(d *gorm.DB) {
		d.Model(&Msg{}).Where("id = ?", msgId).
			Updates(map[string]interface{}{
				"a_model":    aModel,
				"a":          a,
				"a_fil":      aFil,
				"tokens_in":  tokIn,
				"tokens_out": tokOut,
				"ttft":       ttftMS,
				"tps":        tps,
				"a_at":       nowMS(),
			})
	})
}

// computeTPS: OUTPUT phase rate tokens_out*1000/(dur-ttft). When that window is
// under 500ms the model dumped everything at once (burst) and the phase rate is
// meaningless (e.g. 1264 tokens in 15ms → 84000+/s) — fall back to the WHOLE
// duration. ttft=0 (unknown / non-stream) also uses the whole duration.
func computeTPS(tokOut int, ttftMS, durMS int64) int64 {
	if tokOut <= 0 || durMS <= 0 {
		return 0
	}
	gen := durMS - ttftMS
	if gen < 500 {
		gen = durMS
	}
	if gen <= 0 {
		gen = durMS
	}
	return int64(tokOut) * 1000 / gen
}

func touchSen(userId int, convId int64) {
	userTx(userId, func(d *gorm.DB) {
		d.Model(&Sen{}).Where("id = ?", convId).Update("updated_at", now())
	})
}

// senExists reports whether convId lives in this user's DB. Since every user has
// a separate DB, this is also the ownership check.
func senExists(userId int, convId int64) bool {
	if convId == 0 {
		return false
	}
	d, err := shard.get(userId)
	if err != nil {
		return false
	}
	var count int64
	d.Model(&Sen{}).Where("id = ?", convId).Count(&count)
	return count > 0
}

func getMessages(userId int, convId int64) ([]Msg, error) {
	d, err := shard.get(userId)
	if err != nil {
		return nil, err
	}
	var conv Sen
	if err := d.Where("id = ?", convId).First(&conv).Error; err != nil {
		return nil, err // gorm.ErrRecordNotFound if it isn't this user's
	}
	var msgs []Msg
	d.Where("sen_id = ?", convId).Order("id asc").Find(&msgs)
	return msgs, nil
}

func deleteSen(userId int, convId int64) error {
	d, err := shard.get(userId)
	if err != nil {
		return err
	}
	res := d.Where("id = ?", convId).Delete(&Sen{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return d.Where("sen_id = ?", convId).Delete(&Msg{}).Error
}

func renameSen(userId int, convId int64, title string) error {
	d, err := shard.get(userId)
	if err != nil {
		return err
	}
	res := d.Model(&Sen{}).Where("id = ?", convId).Update("title", title)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ---- small helpers ----

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "anon"
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '\x00' {
			return '_'
		}
		return r
	}, s)
}

func extOf(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != "" {
		return ext
	}
	return ".bin"
}
