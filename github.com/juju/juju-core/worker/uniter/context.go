// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package uniter

import (
	"bufio"
	"fmt"
	"io"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/worker/uniter/jujuc"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// HookContext is the implementation of jujuc.Context.
type HookContext struct {
	unit *state.Unit

	// configSettings holds the service configuration.
	configSettings charm.Settings

	// id identifies the context.
	id string

	// uuid is the universally unique identifier of the environment.
	uuid string

	// relationId identifies the relation for which a relation hook is
	// executing. If it is -1, the context is not running a relation hook;
	// otherwise, its value must be a valid key into the relations map.
	relationId int

	// remoteUnitName identifies the changing unit of the executing relation
	// hook. It will be empty if the context is not running a relation hook,
	// or if it is running a relation-broken hook.
	remoteUnitName string

	// relations contains the context for every relation the unit is a member
	// of, keyed on relation id.
	relations map[int]*ContextRelation

	// apiAddrs contains the API server addresses.
	apiAddrs []string
}

func NewHookContext(unit *state.Unit, id, uuid string, relationId int,
	remoteUnitName string, relations map[int]*ContextRelation,
	apiAddrs []string) *HookContext {
	return &HookContext{
		unit:           unit,
		id:             id,
		uuid:           uuid,
		relationId:     relationId,
		remoteUnitName: remoteUnitName,
		relations:      relations,
		apiAddrs:       apiAddrs,
	}
}

func (ctx *HookContext) UnitName() string {
	return ctx.unit.Name()
}

func (ctx *HookContext) PublicAddress() (string, bool) {
	return ctx.unit.PublicAddress()
}

func (ctx *HookContext) PrivateAddress() (string, bool) {
	return ctx.unit.PrivateAddress()
}

func (ctx *HookContext) OpenPort(protocol string, port int) error {
	return ctx.unit.OpenPort(protocol, port)
}

func (ctx *HookContext) ClosePort(protocol string, port int) error {
	return ctx.unit.ClosePort(protocol, port)
}

func (ctx *HookContext) ConfigSettings() (charm.Settings, error) {
	if ctx.configSettings == nil {
		var err error
		ctx.configSettings, err = ctx.unit.ConfigSettings()
		if err != nil {
			return nil, err
		}
	}
	result := charm.Settings{}
	for name, value := range ctx.configSettings {
		result[name] = value
	}
	return result, nil
}

func (ctx *HookContext) HookRelation() (jujuc.ContextRelation, bool) {
	return ctx.Relation(ctx.relationId)
}

func (ctx *HookContext) RemoteUnitName() (string, bool) {
	return ctx.remoteUnitName, ctx.remoteUnitName != ""
}

func (ctx *HookContext) Relation(id int) (jujuc.ContextRelation, bool) {
	r, found := ctx.relations[id]
	return r, found
}

func (ctx *HookContext) RelationIds() []int {
	ids := []int{}
	for id := range ctx.relations {
		ids = append(ids, id)
	}
	return ids
}

// hookVars returns an os.Environ-style list of strings necessary to run a hook
// such that it can know what environment it's operating in, and can call back
// into ctx.
func (ctx *HookContext) hookVars(charmDir, toolsDir, socketPath string) []string {
	vars := []string{
		"APT_LISTCHANGES_FRONTEND=none",
		"DEBIAN_FRONTEND=noninteractive",
		"PATH=" + toolsDir + ":" + os.Getenv("PATH"),
		"CHARM_DIR=" + charmDir,
		"JUJU_CONTEXT_ID=" + ctx.id,
		"JUJU_AGENT_SOCKET=" + socketPath,
		"JUJU_UNIT_NAME=" + ctx.unit.Name(),
		"JUJU_ENV_UUID=" + ctx.uuid,
		"JUJU_API_ADDRESSES=" + strings.Join(ctx.apiAddrs, " "),
	}
	if r, found := ctx.HookRelation(); found {
		vars = append(vars, "JUJU_RELATION="+r.Name())
		vars = append(vars, "JUJU_RELATION_ID="+r.FakeId())
		name, _ := ctx.RemoteUnitName()
		vars = append(vars, "JUJU_REMOTE_UNIT="+name)
	}
	return vars
}

// RunHook executes a hook in an environment which allows it to to call back
// into ctx to execute jujuc tools.
func (ctx *HookContext) RunHook(hookName, charmDir, toolsDir, socketPath string) error {
	ps := exec.Command(filepath.Join(charmDir, "hooks", hookName))
	ps.Env = ctx.hookVars(charmDir, toolsDir, socketPath)
	ps.Dir = charmDir
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("cannot make logging pipe: %v", err)
	}
	ps.Stdout = outWriter
	ps.Stderr = outWriter
	logger := &hookLogger{
		r:    outReader,
		done: make(chan struct{}),
	}
	go logger.run()
	err = ps.Start()
	outWriter.Close()
	if err == nil {
		err = ps.Wait()
	}
	logger.stop()
	if ee, ok := err.(*exec.Error); ok && err != nil {
		if os.IsNotExist(ee.Err) {
			// Missing hook is perfectly valid, but worth mentioning.
			log.Infof("worker/uniter: skipped %q hook (not implemented)", hookName)
			return nil
		}
	}
	write := err == nil
	for id, rctx := range ctx.relations {
		if write {
			if e := rctx.WriteSettings(); e != nil {
				e = fmt.Errorf(
					"could not write settings from %q to relation %d: %v",
					hookName, id, e,
				)
				log.Errorf("worker/uniter: %v", e)
				if err == nil {
					err = e
				}
			}
		}
		rctx.ClearCache()
	}
	return err
}

type hookLogger struct {
	r       io.ReadCloser
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
}

func (l *hookLogger) run() {
	defer close(l.done)
	defer l.r.Close()
	br := bufio.NewReaderSize(l.r, 4096)
	for {
		line, _, err := br.ReadLine()
		if err != nil {
			if err != io.EOF {
				log.Errorf("worker/uniter: cannot read hook output: %v", err)
			}
			break
		}
		l.mu.Lock()
		if l.stopped {
			l.mu.Unlock()
			return
		}
		log.Infof("worker/uniter: HOOK %s", line)
		l.mu.Unlock()
	}
}

func (l *hookLogger) stop() {
	// We can see the process exit before the logger has processed
	// all its output, so allow a moment for the data buffered
	// in the pipe to be processed. We don't wait indefinitely though,
	// because the hook may have started a background process
	// that keeps the pipe open.
	select {
	case <-l.done:
	case <-time.After(100 * time.Millisecond):
	}
	// We can't close the pipe asynchronously, so just
	// stifle output instead.
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
}

// SettingsMap is a map from unit name to relation settings.
type SettingsMap map[string]map[string]interface{}

// ContextRelation is the implementation of jujuc.ContextRelation.
type ContextRelation struct {
	ru *state.RelationUnit

	// members contains settings for known relation members. Nil values
	// indicate members whose settings have not yet been cached.
	members SettingsMap

	// settings allows read and write access to the relation unit settings.
	settings *state.Settings

	// cache is a short-term cache that enables consistent access to settings
	// for units that are not currently participating in the relation. Its
	// contents should be cleared whenever a new hook is executed.
	cache SettingsMap
}

// NewContextRelation creates a new context for the given relation unit.
// The unit-name keys of members supplies the initial membership.
func NewContextRelation(ru *state.RelationUnit, members map[string]int64) *ContextRelation {
	ctx := &ContextRelation{ru: ru, members: SettingsMap{}}
	for unit := range members {
		ctx.members[unit] = nil
	}
	ctx.ClearCache()
	return ctx
}

// WriteSettings persists all changes made to the unit's relation settings.
func (ctx *ContextRelation) WriteSettings() (err error) {
	if ctx.settings != nil {
		_, err = ctx.settings.Write()
	}
	return
}

// ClearCache discards all cached settings for units that are not members
// of the relation, and all unwritten changes to the unit's relation settings.
// including any changes to Settings that have not been written.
func (ctx *ContextRelation) ClearCache() {
	ctx.settings = nil
	ctx.cache = make(SettingsMap)
}

// UpdateMembers ensures that the context is aware of every supplied member
// unit. For each supplied member that has non-nil settings, the cached
// settings will be overwritten; but nil settings will not overwrite cached
// ones.
func (ctx *ContextRelation) UpdateMembers(members SettingsMap) {
	for m, s := range members {
		_, found := ctx.members[m]
		if !found || s != nil {
			ctx.members[m] = s
		}
	}
}

// DeleteMember drops the membership and cache of a single remote unit, without
// perturbing settings for the remaining members.
func (ctx *ContextRelation) DeleteMember(unitName string) {
	delete(ctx.members, unitName)
}

func (ctx *ContextRelation) Id() int {
	return ctx.ru.Relation().Id()
}

func (ctx *ContextRelation) Name() string {
	return ctx.ru.Endpoint().Name
}

func (ctx *ContextRelation) FakeId() string {
	return fmt.Sprintf("%s:%d", ctx.Name(), ctx.ru.Relation().Id())
}

func (ctx *ContextRelation) UnitNames() (units []string) {
	for unit := range ctx.members {
		units = append(units, unit)
	}
	sort.Strings(units)
	return units
}

func (ctx *ContextRelation) Settings() (jujuc.Settings, error) {
	if ctx.settings == nil {
		node, err := ctx.ru.Settings()
		if err != nil {
			return nil, err
		}
		ctx.settings = node
	}
	return ctx.settings, nil
}

func (ctx *ContextRelation) ReadSettings(unit string) (settings map[string]interface{}, err error) {
	settings, member := ctx.members[unit]
	if settings == nil {
		if settings = ctx.cache[unit]; settings == nil {
			settings, err = ctx.ru.ReadSettings(unit)
			if err != nil {
				return nil, err
			}
		}
	}
	if member {
		ctx.members[unit] = settings
	} else {
		ctx.cache[unit] = settings
	}
	return settings, nil
}
