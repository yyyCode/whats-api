// Package config 是反射驱动的配置注册表。
//
// 需求是：几百个配置项，要能在运行时热更新、能通过控制台修改、能多节点同步，
// 还要有类型与校验。常见做法是 yaml 文件 + 手写解析 + 手写校验 + 手写默认值
// + 手写文档，五份东西要同步维护，必然漂移。
//
// 这里把五份合成一份：
//
//	类型       ← Go 结构体字段类型
//	默认值     ← 变量初始值
//	持久化 key ← 模块名 + json tag（自动生成，不会手写错）
//	文档       ← 结构体与字段注释
//
// 注册发生在 init() 期，所以任何用法错误都直接 panic：注册错误属于开发期问题，
// 越早炸越好。
package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/yyyCode/whats-api/internal/common"
)

// typeLabel 给匿名结构体一个可读的名字：elem.Type().Name() 对它返回空串，
// 报错信息就成了「字段 .B」，看不出是哪个模块出的问题。
func typeLabel(t reflect.Type) string {
	if n := t.Name(); n != "" {
		return n
	}
	return t.String()
}

// 支持的字段类型（M0）。扩展这里的同时要扩展 setField 与 encodeField。
var supportedKinds = map[reflect.Kind]bool{
	reflect.String:  true,
	reflect.Bool:    true,
	reflect.Int:     true,
	reflect.Int64:   true,
	reflect.Float64: true,
}

type field struct {
	jsonName string
	index    int
}

type module struct {
	value   reflect.Value // 注册时传入的结构体指针
	fields  map[string]field
	rebuild []func()
}

// Manager 是配置注册表本体。
type Manager struct {
	mu      sync.RWMutex
	modules map[string]*module
}

// GlobalConfig 是进程级单例：配置域在各自的 init() 里往它注册。
// 这是"进程级单例"，只在注册期写入，之后只读。
var GlobalConfig = NewManager()

func NewManager() *Manager {
	return &Manager{modules: map[string]*module{}}
}

// Register 注册一个配置域，必须在 init() 中调用。
//
// 违反约定一律 panic：模块名为空/重复、不是非 nil 结构体指针、
// 导出字段缺 json tag、json tag 重复、字段类型不支持。
func (m *Manager) Register(name string, cfg any) {
	if name == "" {
		panic("config: 模块名不能为空")
	}

	if strings.Contains(name, ".") {
		panic(fmt.Sprintf("config: 模块名 %q 不能含 '.'——配置 key 是「模块名.json tag」，"+
			"名字里的点会让 key 拆不开，不同模块会撞进同一个 key", name))
	}

	rv := reflect.ValueOf(cfg)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic(fmt.Sprintf("config: Register(%q) 需要非 nil 的结构体指针，得到 %T", name, cfg))
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		panic(fmt.Sprintf("config: Register(%q) 需要指向结构体的指针，得到 %s", name, elem.Kind()))
	}

	fields := make(map[string]field, elem.NumField())
	for i := 0; i < elem.NumField(); i++ {
		sf := elem.Type().Field(i)
		if !sf.IsExported() {
			continue // 未导出字段不参与配置（也不报错：它可能只是内部状态）
		}

		rawTag := sf.Tag.Get("json")
		// json:"-" 是 Go 里"明确排除该字段"的写法（json:"-," 才是名为 "-" 的字段），
		// 不是漏写 tag，所以按未导出字段同样跳过而不是报错。
		if rawTag == "-" {
			continue
		}
		jsonName := strings.Split(rawTag, ",")[0]
		if jsonName == "" {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s.%s 缺少 json tag——"+
				"配置 key 由「模块名.json tag」生成，缺 tag 会让持久化 key 漂移",
				name, typeLabel(elem.Type()), sf.Name))
		}
		if strings.Contains(jsonName, ".") {
			panic(fmt.Sprintf("config: 模块 %q 的 json tag %q 不能含 '.'——"+
				"它就是 key 的分隔符", name, jsonName))
		}
		if _, dup := fields[jsonName]; dup {
			panic(fmt.Sprintf("config: 模块 %q 的 json tag %q 重复", name, jsonName))
		}
		if !supportedKinds[sf.Type.Kind()] {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s 类型 %s 不支持"+
				"（M0 支持 string/bool/int/int64/float64）", name, sf.Name, sf.Type.Kind()))
		}
		// 非有限的浮点默认值必须在注册期就炸：json.Marshal 编不出 NaN/±Inf，
		// MustMarshalString 会返回空串，于是 ExportAllConfigs（Option 表默认值的
		// 来源）里出现一个不是 JSON 的 ""，等别处解析才报错，现场已经远离原因。
		if sf.Type.Kind() == reflect.Float64 {
			if f := elem.Field(i).Float(); math.IsNaN(f) || math.IsInf(f, 0) {
				panic(fmt.Sprintf("config: 模块 %q 的字段 %s 默认值是 %v，无法序列化成 JSON"+
					"（NaN/±Inf 不是合法 JSON 数值）", name, sf.Name, f))
			}
		}
		fields[jsonName] = field{jsonName: jsonName, index: i}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.modules[name]; dup {
		panic(fmt.Sprintf("config: 模块 %q 重复注册", name))
	}
	m.modules[name] = &module{value: rv, fields: fields}
}

// OnRebuild 注册该域的快照重建回调，由 UpdateConfig 在写锁内调用。
//
// 回调里应该做「值拷贝 + atomic.Pointer.Store」：于是热路径读到的永远是一个
// 完整一致的版本，而不是正在被反射写入的中间态。
func (m *Manager) OnRebuild(name string, hook func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.modules[name]
	if !ok {
		panic(fmt.Sprintf("config: OnRebuild(%q) 的模块尚未注册", name))
	}
	mod.rebuild = append(mod.rebuild, hook)
}

// ModuleKey 生成持久化 key：<模块名>.<jsonField>。
func ModuleKey(moduleName, jsonName string) string { return moduleName + "." + jsonName }

// ExportAllConfigs 返回所有注册域的**当前值**（初始化阶段即默认值），
// key = <module>.<jsonField>，value = JSON 编码。
func (m *Manager) ExportAllConfigs() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]string, m.fieldCountLocked())
	for name, mod := range m.modules {
		elem := mod.value.Elem()
		for jsonName, f := range mod.fields {
			out[ModuleKey(name, jsonName)] = encodeField(elem.Field(f.index))
		}
	}
	return out
}

// lookupLocked 在持锁状态下解析 key。
//
// ★ 调用约定：必须在持锁时调用。它返回模块元数据与字段下标，
// 而**写入**由调用方自己做——ValidateConfig 持读锁（它不写真实字段），
// UpdateConfig 持写锁（它要写）。把这两个锁语义混在一个函数里，
// 迟早会出现"持读锁却写字段"的并发 bug。
func (m *Manager) lookupLocked(key string) (*module, field, error) {
	modName, jsonName, ok := splitKey(key)
	if !ok {
		return nil, field{}, fmt.Errorf("配置 key %q 格式非法，应为 <module>.<field>", key)
	}
	mod, ok := m.modules[modName]
	if !ok {
		return nil, field{}, fmt.Errorf("未注册的配置模块 %q", modName)
	}
	f, ok := mod.fields[jsonName]
	if !ok {
		return nil, field{}, fmt.Errorf("模块 %q 没有配置项 %q", modName, jsonName)
	}
	return mod, f, nil
}

// GetConfig 返回单个配置项的当前值（JSON 编码）。
func (m *Manager) GetConfig(key string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return "", false
	}
	return encodeField(mod.value.Elem().Field(f.index)), true
}

// ValidateConfig 校验一个新值是否可接受，但**不写入任何状态**。
//
// 为什么它在 UpdateConfig 之外单独存在：配置的真实写入路径是
// 校验 → 落库 → 改内存（见 model.UpdateOption）。校验必须先于落库——
// 否则一个非法值会先落进库里、再被别的实例同步过去，一处错变成处处错。
// 因此"校验"必须能独立于"写入"被调用。
//
// 它把值解析进一个 reflect.New(...).Elem() 临时副本，类型与真实字段**完全相同**，
// 因此不存在"校验通过但写入失败"的裂缝：能装进副本的值就能装进字段。
func (m *Manager) ValidateConfig(key, rawJSON string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return err
	}

	// 只读字段的 Type（在 RLock 下安全），写入目标是这份临时副本。
	scratch := reflect.New(mod.value.Elem().Field(f.index).Type()).Elem()
	if err := setField(scratch, rawJSON); err != nil {
		return fmt.Errorf("配置项 %q: %w", key, err)
	}
	return nil
}

// UpdateConfig 按 key 写回注册的结构体，并触发该域的快照重建。
//
// 未知 key、类型不符、数值越界都返回错误且**不写入**——
// 管理端的一次非法请求不能把运行中的配置改坏。
//
// 注意它不做"先校验再写入"的两段式：解析本身就是校验，
// 而 setField 保证失败时不部分写入。需要"先确认再落库"的调用方
// （如 model.UpdateOption）用 ValidateConfig 做前置校验。
func (m *Manager) UpdateConfig(key, rawJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return err
	}
	if err := setField(mod.value.Elem().Field(f.index), rawJSON); err != nil {
		return fmt.Errorf("配置项 %q: %w", key, err)
	}

	// 在写锁内重建快照：读者要么看到旧版本、要么看到新版本，不会看到中间态。
	for _, hook := range mod.rebuild {
		hook()
	}
	return nil
}

// Modules 返回已注册的模块名（已排序），供 /api/status 展示。
func (m *Manager) Modules() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.modules))
	for name := range m.modules {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FieldCount 返回注册的配置项总数。
func (m *Manager) FieldCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fieldCountLocked()
}

func (m *Manager) fieldCountLocked() int {
	n := 0
	for _, mod := range m.modules {
		n += len(mod.fields)
	}
	return n
}

// splitKey 把 "module.field" 拆开。注意字段名本身可能含点（json tag 允许），
// 所以按**第一个**点切分。
func splitKey(key string) (moduleName, jsonName string, ok bool) {
	idx := strings.Index(key, ".")
	if idx <= 0 || idx == len(key)-1 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}

func encodeField(fv reflect.Value) string {
	switch fv.Kind() {
	case reflect.String:
		return common.MustMarshalString(fv.String())
	case reflect.Bool:
		return common.MustMarshalString(fv.Bool())
	case reflect.Int, reflect.Int64:
		return common.MustMarshalString(fv.Int())
	case reflect.Float64:
		return common.MustMarshalString(fv.Float())
	default:
		return ""
	}
}

// setField 把 JSON 文本写进字段。类型不符返回错误，绝不部分写入。
func setField(fv reflect.Value, rawJSON string) error {
	if !fv.CanSet() {
		return errors.New("字段不可写")
	}

	// null 必须显式拒掉：json.Unmarshal 对 null 不报错，而下面每种类型都是
	// 先解析到零值局部变量再赋值——于是 null 会把配置静默改成零值
	// （sync_frequency 60 → 0 这种）。管理端的一次非法请求不该悄悄改坏配置。
	// 将来若支持指针类型字段（null 表示"未设置"），要在这里放行。
	if strings.TrimSpace(rawJSON) == "null" {
		return errors.New("null 不是合法取值")
	}

	switch fv.Kind() {
	case reflect.String:
		var v string
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望字符串: %w", err)
		}
		fv.SetString(v)

	case reflect.Bool:
		var v bool
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望布尔值: %w", err)
		}
		fv.SetBool(v)

	case reflect.Int, reflect.Int64:
		var v int64
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望整数: %w", err)
		}
		// 32 位构建上 int 是 int32，SetInt 越界会直接 panic，
		// 所以先查一次。amd64 上这个分支不可达（int 为 64 位），
		// 属于防御性代码，测试不覆盖也不假装覆盖。
		if fv.OverflowInt(v) {
			return fmt.Errorf("整数 %d 超出 %s 的表示范围", v, fv.Kind())
		}
		fv.SetInt(v)

	case reflect.Float64:
		var v float64
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望浮点数: %w", err)
		}
		if fv.OverflowFloat(v) {
			return fmt.Errorf("浮点数 %v 超出 %s 的表示范围", v, fv.Kind())
		}
		fv.SetFloat(v)

	default:
		return fmt.Errorf("不支持的字段类型 %s", fv.Kind())
	}
	return nil
}
