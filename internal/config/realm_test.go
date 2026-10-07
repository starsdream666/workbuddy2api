package config

import "testing"

// TestRealmTaskEnabledDefault：未配置 realm_tasks 时全部继承全局开关（= true），
// 保证旧配置与单线部署行为不变。
func TestRealmTaskEnabledDefault(t *testing.T) {
	s := DefaultSchedule()
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for _, task := range []string{TaskCheckin, TaskTravel, TaskActivity, TaskKeepalive} {
		if !s.RealmTaskEnabled("ai", task) {
			t.Errorf("未配置时 %s 应启用", task)
		}
	}
}

// TestRealmTaskEnabledExplicit：显式 false 关掉某 realm 的单个任务，其余不受影响。
func TestRealmTaskEnabledExplicit(t *testing.T) {
	no := false
	yes := true
	s := DefaultSchedule()
	s.RealmTasks = map[string]RealmTaskFlags{
		"ai": {Checkin: &no, Keepalive: &yes},
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if s.RealmTaskEnabled("ai", TaskCheckin) {
		t.Error("ai.checkin 显式 false 应关")
	}
	if !s.RealmTaskEnabled("ai", TaskKeepalive) {
		t.Error("ai.keepalive 显式 true 应开")
	}
	if !s.RealmTaskEnabled("ai", TaskTravel) {
		t.Error("未列出的字段（travel）应继续继承 = true")
	}
	if !s.RealmTaskEnabled("cn", TaskCheckin) {
		t.Error("cn 未配置，应不受 workbuddy（旧名 ai）的覆盖影响")
	}
}

// TestRealmTaskEnabledUnknownArgs：未知 realm / 任务名不阻碍既有行为（返回 true）。
func TestRealmTaskEnabledUnknownArgs(t *testing.T) {
	s := DefaultSchedule()
	if !s.RealmTaskEnabled("bogus", TaskCheckin) {
		t.Error("未知 realm 应返回 true")
	}
	if !s.RealmTaskEnabled("ai", "bogus-task") {
		t.Error("未知任务名应返回 true")
	}
}

// TestNormalizeRejectsUnknownRealm：realm_tasks 写了未知 realm 一律启动报错
// （避免"配置写了但不生效"的静默陷阱）。
func TestNormalizeRejectsUnknownRealm(t *testing.T) {
	s := DefaultSchedule()
	s.RealmTasks = map[string]RealmTaskFlags{"staging": {}}
	if err := s.Normalize(); err == nil {
		t.Fatal("未知 realm 应报错")
	}
}

// TestRealmTaskEnabledLegacyAlias 旧名与归一名必须查到同一份配置：
// 配置写 "workbuddy"、查询用 "ai"（或反过来）都要命中 —— 这是改名后旧配置继续生效的前提。
func TestRealmTaskEnabledLegacyAlias(t *testing.T) {
	no := false
	s := DefaultSchedule()
	s.RealmTasks = map[string]RealmTaskFlags{
		"workbuddy": {Checkin: &no},
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if s.RealmTaskEnabled("ai", TaskCheckin) {
		t.Error("旧名 ai 查询必须命中 workbuddy 的配置")
	}
	if s.RealmTaskEnabled("workbuddy", TaskCheckin) {
		t.Error("归一名查询必须命中自身配置")
	}
	if !s.RealmTaskEnabled("cn", TaskCheckin) {
		t.Error("cn 不该被 workbuddy 的覆盖影响")
	}
	if !s.RealmTaskEnabled("bogus", TaskCheckin) {
		t.Error("未知 realm 应返回 true（不被归一成 cn 后误继承开关）")
	}
}
