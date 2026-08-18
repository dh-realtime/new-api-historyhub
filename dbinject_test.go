package historyhub

// Dev-only helper for the isolated browser-test environment: copies of
// one-api.db get a local "mock" channel pointing at the mockllm server
// (/data/hyb/mockllm), so end-to-end tests never touch real upstreams.
//
// Skipped unless HYB_INJECT_DB points at a sqlite file:
//
//	HYB_INJECT_DB=/tmp/hybtest/one-api.db go test ./0/ -run 'TestInjectMockChannel|TestCreateTestUser'
import (
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openInjectDB(t *testing.T) *gorm.DB {
	path := os.Getenv("HYB_INJECT_DB")
	if path == "" {
		t.Skip("HYB_INJECT_DB not set")
	}
	d, err := gorm.Open(sqlite.Open(path+"?_busy_timeout=30000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestCreateTestUser inserts a known-credential user (13800000001 /
// hyb-test-123) into the test DB, replacing any previous copy.
func TestCreateTestUser(t *testing.T) {
	d := openInjectDB(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("hyb-test-123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	d.Exec("DELETE FROM users WHERE username = ?", "13800000001")
	aff := "hybtest" + time.Now().Format("150405")
	if err := d.Exec(`INSERT INTO users
		(username, password, display_name, role, status, email, quota, used_quota, request_count, "group", aff_code, created_at, last_login_at)
		VALUES (?, ?, '测试用户', 1, 1, '', 500000000, 0, 0, 'default', ?, ?, 0)`,
		"13800000001", string(hash), aff, time.Now().Unix()).Error; err != nil {
		t.Fatal(err)
	}
	var id int64
	d.Raw("SELECT id FROM users WHERE username = ?", "13800000001").Scan(&id)
	t.Logf("test user id=%d username=13800000001 password=hyb-test-123", id)
	_ = common.Version
}

func TestInjectMockChannel(t *testing.T) {
	d := openInjectDB(t)
	base := "http://127.0.0.1:19999"
	type chRow struct {
		Id     int64
		Name   string
		Status int
	}
	var existing []chRow
	d.Table("channels").Select("id", "name", "status").Where("name = ?", "mock").Find(&existing)
	for _, e := range existing {
		if err := d.Exec("UPDATE channels SET status = 1, base_url = ?, models = ?, \"group\" = 'default' WHERE id = ?", base, "mock-llm", e.Id).Error; err != nil {
			t.Fatal(err)
		}
		d.Exec("DELETE FROM abilities WHERE channel_id = ?", e.Id)
		if err := d.Exec(`INSERT INTO abilities ("group", model, channel_id, enabled, priority, weight, tag)
			VALUES ('default', 'mock-llm', ?, 1, 0, 0, '')`, e.Id).Error; err != nil {
			t.Fatal(err)
		}
		if err := d.Exec(`INSERT INTO options (key, value) VALUES ('SelfUseModeEnabled', 'true')
			ON CONFLICT(key) DO UPDATE SET value = 'true'`).Error; err != nil {
			t.Log("set SelfUseModeEnabled:", err)
		}
		t.Logf("re-enabled mock channel id=%d + ability", e.Id)
		return
	}
	var id int64
	d.Raw("SELECT id FROM channels WHERE name = ?", "mock").Scan(&id)
	if id == 0 {
		t.Fatal("mock channel not found after insert")
	}
	// A raw channel row alone is not routable: new-api resolves models through
	// the abilities table, normally maintained by the channel CRUD.
	d.Exec("DELETE FROM abilities WHERE channel_id = ?", id)
	if err := d.Exec(`INSERT INTO abilities ("group", model, channel_id, enabled, priority, weight, tag)
		VALUES ('default', 'mock-llm', ?, 1, 0, 0, '')`, id).Error; err != nil {
		t.Fatal(err)
	}
	// SelfUseModeEnabled: ListModels hides models without a billing config
	// otherwise — mock-llm has none, so the test instance needs it on.
	if err := d.Exec(`INSERT INTO options (key, value) VALUES ('SelfUseModeEnabled', 'true')
		ON CONFLICT(key) DO UPDATE SET value = 'true'`).Error; err != nil {
		t.Log("set SelfUseModeEnabled:", err)
	}
	t.Logf("inserted mock channel id=%d + ability", id)
}
