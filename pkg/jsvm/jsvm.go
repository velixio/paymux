// Package jsvm runs JavaScript hooks: small scripts in a directory that react
// to Paymux events (payment succeeded, refund succeeded, ...) and may suggest
// an order for provider accounts. It is the scripting counterpart of the Go
// hooks in the hub package, and carries the same guarantees: a script sees
// JSON copies of an event, has no way to move money, and can never hold up or
// roll back a payment.
//
// Scripts are plain ECMAScript run by goja (github.com/dop251/goja). They
// register handlers at top level:
//
//	onPaymentSucceeded(function (e) { ... })
//	onRefundSucceeded, onDisputeOpened, onSubscriptionRenewed, onInvoiceIssued
//	onRoute(function (req) { return ["pa_..."]; })
//
// Sandbox: no filesystem, network, process, module or timer API exists in the
// runtime. The only ways out are console.log (to the Paymux log) and fetch,
// which is limited to the hosts in Options.AllowedHosts, goes through the
// SSRF-guarded client in Options.HTTPClient, takes at most FetchTimeout, and
// reads and writes at most MaxBody bytes. A call may use CallTimeout of script
// time (time blocked in fetch does not count); a script that runs longer is
// interrupted and its runtime thrown away.
//
// Memory and output are bounded as well. A call may grow the live heap by at
// most MaxMemory bytes (checked every millisecond; a script over the limit is
// interrupted and its runtime thrown away, like one over the time limit), the
// built-ins that can allocate a huge result in one step (repeat, padStart,
// padEnd, fill, join, concat, Array.from, apply) refuse sizes beyond
// maxStringLen characters and maxArrayLen elements, and what a script says is
// capped: a console message at maxConsoleBytes, console lines per call at
// maxConsoleLines, and an answer to routing at maxRouteEntries entries. The
// heap is the process's, so the guard is an approximation under load; it
// fails closed. Hooks are code the operator wrote, and a container memory
// limit remains the hard bound.
//
// fetch(url, {method, headers, body}) is synchronous: it returns the response
// {status, ok, headers, text(), json()} or throws. The result can also be
// awaited or chained with .then, so code written for the web works.
package jsvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/dop251/goja"
)

// maxRegexpMatch bounds one regular-expression match in a script. goja runs
// patterns Go's regexp cannot (backreferences, lookaround) on regexp2, whose
// matcher the VM's interrupt does not reach: a backtracking pattern on event
// data would otherwise run past CallTimeout and keep its VM out of the pool.
// The match then fails with an error, which ends the call.
const maxRegexpMatch = time.Second

func init() {
	if regexp2.DefaultMatchTimeout > maxRegexpMatch {
		regexp2.DefaultMatchTimeout = maxRegexpMatch
	}
}

// Defaults for the limits in Options.
const (
	DefaultCallTimeout  = 2 * time.Second
	DefaultFetchTimeout = 5 * time.Second
	DefaultMaxBody      = 1 << 20
	DefaultPollEvery    = 10 * time.Second
	DefaultPoolSize     = 4
	DefaultMaxMemory    = 128 << 20
	maxFetchesPerCall   = 10

	maxStringLen    = 16 << 20 // characters a built-in may build in one step
	maxArrayLen     = 1 << 22  // elements a built-in may touch in one step
	maxConsoleBytes = 8 << 10  // per console message
	maxConsoleLines = 100      // per hook call
	maxResultItems  = 1024     // length of an array a handler may return
	maxRouteEntries = 64       // accounts in a routing answer
	maxRouteEntry   = 256      // characters in one
)

// Options configures a Runtime.
type Options struct {
	// Dir holds the *.js files (not searched recursively).
	Dir string
	// HTTPClient performs fetch. It must be the SSRF-guarded client
	// (outbound.GuardedClient) so that private addresses are refused at dial
	// time; AllowedHosts is checked in addition.
	HTTPClient *http.Client
	// AllowedHosts are the hosts fetch may call: exact names, or "*.example.com"
	// for any subdomain. Empty means fetch is refused everywhere.
	AllowedHosts []string
	Logger       *slog.Logger
	// OnError is told about files that fail to compile or run at load time
	// (hook is the file name). Errors of calls are returned to the caller.
	OnError func(hook string, err error)

	PoolSize     int           // runtimes per generation (default 4)
	CallTimeout  time.Duration // script time per call (default 2s)
	FetchTimeout time.Duration // per fetch (default 5s)
	MaxBody      int64         // request and response body cap in bytes (default 1 MiB)
	MaxMemory    int64         // live heap one call may add, in bytes (default 128 MiB)
	PollEvery    time.Duration // file change check interval for Watch (default 10s)
}

// eventHooks are the hooks Call accepts; onRoute is Route.
var eventHooks = map[string]bool{
	"onPaymentSucceeded": true, "onRefundSucceeded": true, "onDisputeOpened": true,
	"onSubscriptionRenewed": true, "onInvoiceIssued": true,
}

const routeHook = "onRoute"

// Runtime is a set of loaded scripts and a pool of VMs to run them.
type Runtime struct {
	o Options

	mu       sync.RWMutex
	gen      *generation
	sig      string                   // signature of the files gen was built from
	programs map[string]*goja.Program // last good compiled program per file
	hookList []string                 // hooks the current scripts register
	closed   bool
	reloadMu sync.Mutex
}

// New loads the scripts in o.Dir. Files that fail are reported through
// o.OnError and skipped; New fails only on unusable options.
func New(o Options) (*Runtime, error) {
	if o.Dir == "" {
		return nil, errors.New("jsvm: Dir is required")
	}
	if o.HTTPClient == nil {
		return nil, errors.New("jsvm: HTTPClient is required (use outbound.GuardedClient)")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PoolSize <= 0 {
		o.PoolSize = DefaultPoolSize
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	if o.MaxBody <= 0 {
		o.MaxBody = DefaultMaxBody
	}
	if o.MaxMemory <= 0 {
		o.MaxMemory = DefaultMaxMemory
	}
	if o.PollEvery <= 0 {
		o.PollEvery = DefaultPollEvery
	}
	for i, h := range o.AllowedHosts {
		o.AllowedHosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	r := &Runtime{o: o, programs: map[string]*goja.Program{}}
	if err := r.Reload(); err != nil {
		o.Logger.Warn("js hooks: some files could not be loaded", "err", err)
	}
	return r, nil
}

// Close stops the runtime; calls in progress finish.
func (r *Runtime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.gen != nil {
		r.gen.close()
	}
}

// Hooks lists the hooks the loaded scripts register, sorted.
func (r *Runtime) Hooks() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.hookList...)
}

// Has reports whether any loaded script registers the hook.
func (r *Runtime) Has(hook string) bool {
	for _, h := range r.Hooks() {
		if h == hook {
			return true
		}
	}
	return false
}

func (r *Runtime) report(hook string, err error) {
	r.o.Logger.Error("js hooks: load failed", "file", hook, "err", err)
	if r.o.OnError != nil {
		r.o.OnError(hook, err)
	}
}

// signature describes the *.js files (name, size, modification time).
func (r *Runtime) signature() (string, []string, error) {
	entries, err := os.ReadDir(r.o.Dir)
	if err != nil {
		return "", nil, err
	}
	var names []string
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		names = append(names, e.Name())
		fmt.Fprintf(&b, "%s|%d|%d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
	}
	sort.Strings(names)
	return b.String(), names, nil
}

// Changed reports whether the files differ from what is loaded.
func (r *Runtime) Changed() bool {
	sig, _, err := r.signature()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return err == nil && sig != r.sig
}

// Reload reads the directory again and swaps in new VMs. A file that no longer
// compiles is reported and keeps its last good version; a file that was
// removed is dropped. The returned error lists the files that failed.
func (r *Runtime) Reload() error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	sig, names, err := r.signature()
	if err != nil {
		return err
	}
	var errs []error
	progs := map[string]*goja.Program{}
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join(r.o.Dir, name))
		if err == nil {
			var p *goja.Program
			if p, err = goja.Compile(name, string(src), false); err == nil {
				progs[name] = p
				continue
			}
		}
		err = fmt.Errorf("%s: %w", name, err)
		errs = append(errs, err)
		r.report(name, err)
		r.mu.RLock()
		if old, ok := r.programs[name]; ok {
			progs[name] = old // keep the last good version
		}
		r.mu.RUnlock()
	}
	ordered := make([]namedProgram, 0, len(progs))
	for _, name := range names {
		if p, ok := progs[name]; ok {
			ordered = append(ordered, namedProgram{name, p})
		}
	}
	g := newGeneration(r, ordered)
	first, loadErrs := g.build(context.Background())
	for _, le := range loadErrs {
		err := fmt.Errorf("%s: %w", le.file, le.err)
		errs = append(errs, err)
		r.report(le.file, err)
	}
	g.tokens <- struct{}{}
	g.free <- first
	var hooks []string
	for h, hs := range first.handlers {
		if len(hs) > 0 {
			hooks = append(hooks, h)
		}
	}
	sort.Strings(hooks)

	r.mu.Lock()
	old := r.gen
	r.gen, r.sig, r.programs, r.hookList = g, sig, progs, hooks
	closed := r.closed
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	if closed {
		g.close()
	}
	if old != nil {
		r.o.Logger.Info("js hooks reloaded", "files", len(ordered), "hooks", hooks)
	} else {
		r.o.Logger.Info("js hooks loaded", "files", len(ordered), "hooks", hooks)
	}
	return errors.Join(errs...)
}

// Watch reloads the scripts when their files change, checking every
// Options.PollEvery, until ctx ends.
func (r *Runtime) Watch(ctx context.Context) {
	t := time.NewTicker(r.o.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.Changed() {
				_ = r.Reload() // failures are reported inside
			}
		}
	}
}

// ---- generations and VMs ----

type namedProgram struct {
	name string
	prog *goja.Program
}

// generation is one load of the scripts with its own pool of VMs. A reload
// makes a new generation; VMs of the old one finish their call and are
// dropped.
type generation struct {
	r        *Runtime
	programs []namedProgram
	free     chan *instance
	tokens   chan struct{} // one per live or in-creation VM, capacity PoolSize
	closed   atomic.Bool
}

func newGeneration(r *Runtime, programs []namedProgram) *generation {
	return &generation{r: r, programs: programs, free: make(chan *instance, r.o.PoolSize), tokens: make(chan struct{}, r.o.PoolSize)}
}

func (g *generation) close() {
	g.closed.Store(true)
	for {
		select {
		case <-g.free:
			<-g.tokens
		default:
			return
		}
	}
}

// acquire returns a VM, creating one if the pool is not full, else waiting.
func (g *generation) acquire(ctx context.Context) (*instance, error) {
	select {
	case in := <-g.free:
		return in, nil
	default:
	}
	select {
	case in := <-g.free:
		return in, nil
	case g.tokens <- struct{}{}:
		in, _ := g.build(ctx) // load errors were reported when the generation was made
		return in, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release returns a VM to the pool, or drops it (discard, or the generation is
// retired).
func (g *generation) release(in *instance, discard bool) {
	if discard || g.closed.Load() {
		<-g.tokens
		return
	}
	select {
	case g.free <- in:
	default:
		<-g.tokens
	}
}

type fileErr struct {
	file string
	err  error
}

type instance struct {
	g         *generation
	vm        *goja.Runtime
	handlers  map[string][]goja.Callable
	jsonParse goja.Callable
	jsonStr   goja.Callable
	cur       *callState
}

// callState is the context of one call: its context, fetch count and script
// time budget.
type callState struct {
	ctx     context.Context
	fetches int
	lines   int // console lines written so far
	b       *budget
}

// build makes a VM and runs every script in it. A script that throws is
// recorded and the rest still run. A script that exceeds the time limit
// leaves the VM interrupted, so the VM is built again without it.
func (g *generation) build(ctx context.Context) (*instance, []fileErr) {
	skip := map[string]bool{}
	var errs []fileErr
	seen := map[string]bool{}
	note := func(name string, err error) {
		if !seen[name] {
			seen[name] = true
			errs = append(errs, fileErr{name, err})
		}
	}
	for {
		in := &instance{g: g, vm: goja.New(), handlers: map[string][]goja.Callable{}}
		in.install()
		tainted := false
		for _, p := range g.programs {
			if skip[p.name] {
				continue
			}
			b := newBudget(in.vm, g.r.o.CallTimeout, g.r.o.MaxMemory)
			in.cur = &callState{ctx: ctx, b: b}
			_, err := in.vm.RunProgram(p.prog)
			timedOut := b.stop()
			in.cur = nil
			switch {
			case timedOut:
				note(p.name, b.limitError(g.r.o.CallTimeout, g.r.o.MaxMemory))
				skip[p.name] = true
				tainted = true
			case err != nil:
				note(p.name, cleanErr(err))
			}
			if tainted {
				break
			}
		}
		if !tainted {
			return in, errs
		}
	}
}

func errTimeLimit(d time.Duration) error {
	return fmt.Errorf("hook exceeded the time limit of %v", d)
}

// limitError says which limit stopped the call.
func (b *budget) limitError(d time.Duration, mem int64) error {
	if b.memory.Load() {
		return fmt.Errorf("hook exceeded the memory limit of %d MiB", mem>>20)
	}
	return errTimeLimit(d)
}

// cleanErr turns a goja error into the message a script author wants to see.
func cleanErr(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return errors.New(ex.Value().String())
	}
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return errors.New("interrupted")
	}
	return err
}

// ---- time budget ----

// budget limits the script time of a call. Time spent blocked in a Go function
// such as fetch is not counted (pause/resume around it).
type budget struct {
	vm        *goja.Runtime
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
	fired     atomic.Bool // the time or the memory limit was hit
	memory    atomic.Bool // ... and it was the memory limit
	stopped   bool
	memStop   chan struct{}
	memDone   chan struct{}
}

func newBudget(vm *goja.Runtime, limit time.Duration, memory int64) *budget {
	b := &budget{vm: vm, remaining: limit}
	b.resume()
	b.watchMemory(memory)
	return b
}

// heapObjects is the bytes held by heap objects, live or not yet swept.
func heapObjects() uint64 { return readHeap("/memory/classes/heap/objects:bytes") }

// heapLive is the heap the last garbage collection found live.
func heapLive() uint64 { return readHeap("/gc/heap/live:bytes") }

func readHeap(name string) uint64 {
	s := []metrics.Sample{{Name: name}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindUint64 {
		return s[0].Value.Uint64()
	}
	return 0
}

// gcBaselineAt is the heap size from which a call starts with a collection, so
// that the baseline is the live heap and not the live heap of some earlier moment.
const gcBaselineAt = 32 << 20

// baselineHeap is the live heap now. A small heap needs no collection to know
// it (the live heap cannot be larger than all heap objects); a bigger one is
// collected first, since the last collection may have seen the memory of an
// earlier script that is garbage by now.
func baselineHeap() uint64 {
	if heapObjects() >= gcBaselineAt {
		runtime.GC()
	}
	return min(heapLive(), heapObjects())
}

// watchMemory interrupts the VM when the call has added more than limit bytes
// to the live heap. The baseline is the live heap when the call starts (the
// process may hold plenty of garbage then, and that must not count). The cheap count of heap objects includes garbage, so only when it
// passes the baseline plus the limit is a collection forced and the live heap
// measured again: memory that survives it is what the script holds.
func (b *budget) watchMemory(limit int64) {
	if limit <= 0 {
		return
	}
	base := baselineHeap()
	b.memStop, b.memDone = make(chan struct{}), make(chan struct{})
	kill := func() {
		b.memory.Store(true)
		b.fired.Store(true)
		b.vm.Interrupt("memory limit")
	}
	go func() {
		defer close(b.memDone)
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		lim := uint64(limit)
		next := base + lim
		// The heap is the process's, so growth over the limit may not be the
		// script's (another goroutine took memory meanwhile). Over the limit, a
		// script is stopped when the heap keeps growing while it runs (a runaway
		// does; memory taken once by something else does not), or at once when it
		// is far over (three times the limit).
		var suspect, settled bool
		var seenLive, seenObjects uint64
		var since time.Time
		for {
			select {
			case <-b.memStop:
				return
			case <-t.C:
			}
			if suspect {
				// Far over: no waiting to see whether it keeps growing, and no collection
				// either (a collection cannot start while the script is inside a long
				// built-in call, so this would come too late). Garbage the collector has
				// not freed yet is allowed for by adding the last live size.
				if heapObjects() > base+3*lim+max(heapLive(), base) {
					kill()
					return
				}
				if time.Since(since) > 5*time.Second {
					suspect, next = false, heapObjects()+lim/2
					continue
				}
				// Growth in the first moments may be a burst of somebody else's. After
				// them the heap is measured again, and only growth beyond that counts: a
				// runaway is still growing, a burst is over.
				if !settled {
					if time.Since(since) < 150*time.Millisecond {
						continue
					}
					runtime.GC()
					seenLive, seenObjects, settled = heapLive(), heapObjects(), true
					continue
				}
				if heapObjects() < seenObjects+lim/16 {
					continue
				}
				runtime.GC()
				live := heapLive()
				if live >= seenLive+lim/16 && live > base+lim {
					kill()
					return
				}
				suspect, next = false, heapObjects()+lim/2
				continue
			}
			if heapObjects() <= next {
				continue
			}
			runtime.GC()
			live := heapLive()
			switch {
			case live > base+3*lim:
				kill()
				return
			case live > base+lim:
				suspect, settled, seenLive, seenObjects, since = true, false, live, heapObjects(), time.Now()
			default: // mostly garbage: look again after another half limit of growth
				next = heapObjects() + lim/2
			}
		}
	}()
}

func (b *budget) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || b.fired.Load() {
		return
	}
	b.started = time.Now()
	b.timer = time.AfterFunc(b.remaining, func() {
		b.fired.Store(true)
		b.vm.Interrupt("time limit")
	})
}

func (b *budget) pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil && b.timer.Stop() {
		b.remaining -= time.Since(b.started)
		if b.remaining < time.Millisecond {
			b.remaining = time.Millisecond
		}
	}
}

// stop ends the budget and reports whether the limit was hit, in which case
// the VM has been interrupted and must not be reused.
func (b *budget) stop() (timedOut bool) {
	b.mu.Lock()
	b.stopped = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	if b.memStop != nil {
		close(b.memStop)
		<-b.memDone
	}
	return b.fired.Load()
}

// ---- calling hooks ----

// Call runs every handler the scripts registered for an event hook, in file
// order, each with its own copy of the event (any JSON-serialisable value).
// All handlers run even if one fails; the errors are joined. A hook with no
// handler is not an error.
func (r *Runtime) Call(ctx context.Context, hook string, event any) error {
	if !eventHooks[hook] {
		return fmt.Errorf("jsvm: unknown hook %q", hook)
	}
	_, err := r.invoke(ctx, hook, event)
	return err
}

// Route asks the onRoute handlers for an order of provider accounts and
// returns the first non-empty answer (strings only). With no suggestion it
// returns nil, and the error of the first handler that failed, if any; callers
// treat an error as "no opinion".
func (r *Runtime) Route(ctx context.Context, req any) ([]string, error) {
	vals, err := r.invoke(ctx, routeHook, req)
	for _, v := range vals {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		var out []string
		for _, x := range list {
			if s, ok := x.(string); ok && s != "" && len(s) <= maxRouteEntry && len(out) < maxRouteEntries {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, err
}

func (r *Runtime) invoke(ctx context.Context, hook string, arg any) ([]any, error) {
	r.mu.RLock()
	g, closed := r.gen, r.closed
	r.mu.RUnlock()
	if closed || g == nil || !r.Has(hook) {
		return nil, nil
	}
	payload, err := json.Marshal(arg)
	if err != nil {
		return nil, fmt.Errorf("jsvm: event is not JSON: %w", err)
	}
	in, err := g.acquire(ctx)
	if err != nil {
		return nil, err
	}
	discard := false
	defer func() { g.release(in, discard) }()

	b := newBudget(in.vm, r.o.CallTimeout, r.o.MaxMemory)
	in.cur = &callState{ctx: ctx, b: b}
	var results []any
	var errs []error
	for _, h := range in.handlers[hook] {
		if b.fired.Load() {
			break
		}
		argv, perr := in.jsonParse(goja.Undefined(), in.vm.ToValue(string(payload)))
		if perr != nil {
			errs = append(errs, cleanErr(perr))
			continue
		}
		res, herr := h(goja.Undefined(), argv)
		if b.fired.Load() {
			break
		}
		if herr != nil {
			errs = append(errs, cleanErr(herr))
			continue
		}
		if res != nil && !goja.IsUndefined(res) && !goja.IsNull(res) {
			if err := resultTooBig(res); err != nil {
				errs = append(errs, err)
				continue
			}
			if p, ok := res.Export().(*goja.Promise); ok {
				switch p.State() {
				case goja.PromiseStateRejected:
					errs = append(errs, errors.New(rejectionText(p.Result())))
					continue
				case goja.PromiseStatePending:
					r.o.Logger.Warn("js hooks: a handler returned a promise that never settled; nothing can resolve it here", "hook", hook)
					continue
				default:
					if err := resultTooBig(p.Result()); err != nil {
						errs = append(errs, err)
						continue
					}
					results = append(results, p.Result().Export())
					continue
				}
			}
			results = append(results, res.Export())
		}
	}
	timedOut := b.stop()
	in.cur = nil
	if timedOut {
		discard = true
		errs = append(errs, b.limitError(r.o.CallTimeout, r.o.MaxMemory))
	}
	return results, errors.Join(errs...)
}

// resultTooBig refuses an answer that is an array of more than maxResultItems
// entries, before it is copied out of the VM.
func resultTooBig(v goja.Value) error {
	if o, ok := v.(*goja.Object); ok && o.ClassName() == "Array" {
		if n := o.Get("length"); n != nil && n.ToInteger() > maxResultItems {
			return fmt.Errorf("the hook's answer is too large (%d entries; the limit is %d)", n.ToInteger(), maxResultItems)
		}
	}
	return nil
}

func rejectionText(v goja.Value) string {
	if o, ok := v.(*goja.Object); ok {
		if m := o.Get("message"); m != nil && !goja.IsUndefined(m) {
			return m.String()
		}
	}
	if v == nil {
		return "promise rejected"
	}
	return v.String()
}

// ---- the sandbox: globals ----

const fetchPrelude = `(function (native) {
  globalThis.fetch = function fetch(url, opts) {
    var res = native(String(url), opts);
    res.then = function (ok, fail) {
      var plain = Object.assign({}, res);
      delete plain.then;
      return Promise.resolve(plain).then(ok, fail);
    };
    return res;
  };
})`

// guardPrelude replaces the built-ins that can allocate a huge result in one
// step with versions that refuse sizes beyond the limits. A loop that grows
// memory a little at a time is caught by the memory watch instead.
const guardPrelude = `(function (maxStr, maxArr) {
  function big(what) { throw new RangeError(what + " is too large for a hook (limits: " + maxStr + " characters, " + maxArr + " elements)"); }
  function num(x) { x = Number(x); return x !== x ? 0 : x; }
  function len(o) { return o == null ? 0 : num(o.length); }
  var fapply = Function.prototype.apply; // the original, since apply itself is wrapped below
  function wrap(obj, name, size) {
    var orig = obj[name];
    if (typeof orig !== "function") return;
    Object.defineProperty(obj, name, { configurable: true, writable: true, value: function () {
      fapply.call(size, this, arguments);
      return fapply.call(orig, this, arguments);
    } });
  }
  var S = String.prototype, A = Array.prototype;
  wrap(S, "repeat", function (n) { if (String(this).length * Math.max(0, num(n)) > maxStr) big("a string"); });
  wrap(S, "padStart", function (n) { if (num(n) > maxStr) big("a string"); });
  wrap(S, "padEnd", function (n) { if (num(n) > maxStr) big("a string"); });
  wrap(A, "fill", function () { if (len(this) > maxArr) big("an array"); });
  wrap(A, "join", function (sep) { if (len(this) * (1 + String(sep === undefined ? "," : sep).length) > maxStr) big("a string"); });
  wrap(A, "toString", function () { if (len(this) > maxStr) big("a string"); });
  wrap(A, "concat", function () {
    var n = len(this);
    for (var i = 0; i < arguments.length; i++) n += Array.isArray(arguments[i]) ? len(arguments[i]) : 1;
    if (n > maxArr) big("an array");
  });
  wrap(A, "flat", function () { if (len(this) > maxArr) big("an array"); });
  wrap(A, "slice", function () { if (len(this) > maxArr) big("an array"); });
  wrap(A, "splice", function () { if (len(this) > maxArr) big("an array"); });
  wrap(A, "reverse", function () { if (len(this) > maxArr) big("an array"); });
  wrap(A, "sort", function () { if (len(this) > maxArr) big("an array"); });
  wrap(Array, "from", function (x) { if (len(x) > maxArr) big("an array"); });
  wrap(Function.prototype, "apply", function (t, args) { if (len(args) > maxArr) big("an argument list"); });
  if (typeof Reflect === "object") wrap(Reflect, "apply", function (f, t, args) { if (len(args) > maxArr) big("an argument list"); });
})`

func (in *instance) install() {
	vm := in.vm
	// Hook registration.
	reg := func(name string) {
		_ = vm.Set(name, func(call goja.FunctionCall) goja.Value {
			fn, ok := goja.AssertFunction(call.Argument(0))
			if !ok {
				panic(vm.NewTypeError(name + ": the argument must be a function"))
			}
			in.handlers[name] = append(in.handlers[name], fn)
			return goja.Undefined()
		})
	}
	for name := range eventHooks {
		reg(name)
	}
	reg(routeHook)

	// JSON helpers (also used to hand events over as plain JS objects).
	jsonObj := vm.Get("JSON").ToObject(vm)
	in.jsonParse, _ = goja.AssertFunction(jsonObj.Get("parse"))
	in.jsonStr, _ = goja.AssertFunction(jsonObj.Get("stringify"))

	// console.
	console := vm.NewObject()
	logf := func(level slog.Level) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			if cs := in.cur; cs != nil {
				if cs.lines++; cs.lines > maxConsoleLines {
					if cs.lines == maxConsoleLines+1 {
						in.g.r.o.Logger.Warn("js hook: console output suppressed after the first lines of this call", "source", "js", "limit", maxConsoleLines)
					}
					return goja.Undefined()
				}
			}
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = in.format(a)
			}
			msg := strings.Join(parts, " ")
			if len(msg) > maxConsoleBytes {
				cut := maxConsoleBytes
				for cut > 0 && !utf8.RuneStart(msg[cut]) {
					cut--
				}
				msg = msg[:cut] + "...[truncated]"
			}
			in.g.r.o.Logger.Log(context.Background(), level, "js hook: "+msg, "source", "js")
			return goja.Undefined()
		}
	}
	_ = console.Set("log", logf(slog.LevelInfo))
	_ = console.Set("info", logf(slog.LevelInfo))
	_ = console.Set("debug", logf(slog.LevelDebug))
	_ = console.Set("warn", logf(slog.LevelWarn))
	_ = console.Set("error", logf(slog.LevelError))
	_ = vm.Set("console", console)

	// Limits on what one built-in call may allocate.
	if guard, err := vm.RunString(guardPrelude); err == nil {
		if install, ok := goja.AssertFunction(guard); ok {
			_, _ = install(goja.Undefined(), vm.ToValue(maxStringLen), vm.ToValue(maxArrayLen))
		}
	}

	// fetch.
	prelude, err := vm.RunString(fetchPrelude)
	if err == nil {
		if install, ok := goja.AssertFunction(prelude); ok {
			_, _ = install(goja.Undefined(), vm.ToValue(in.fetchNative))
		}
	}
}

func (in *instance) format(v goja.Value) string {
	if o, ok := v.(*goja.Object); ok && in.jsonStr != nil {
		if s, err := in.jsonStr(goja.Undefined(), o); err == nil && s != nil && !goja.IsUndefined(s) {
			return s.String()
		}
	}
	if v == nil {
		return ""
	}
	return v.String()
}

// hostAllowed reports whether host matches an allowed entry.
func (r *Runtime) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, a := range r.o.AllowedHosts {
		if a == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(a, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

var fetchMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// fetchNative is the Go side of fetch. It throws (a JS Error) when a request
// is refused or fails.
func (in *instance) fetchNative(call goja.FunctionCall) goja.Value {
	vm, r := in.vm, in.g.r
	fail := func(format string, a ...any) goja.Value { panic(vm.NewGoError(fmt.Errorf("fetch: "+format, a...))) }
	cs := in.cur
	if cs == nil {
		return fail("not available outside a hook call")
	}
	if cs.fetches++; cs.fetches > maxFetchesPerCall {
		return fail("too many requests in one hook call (limit %d)", maxFetchesPerCall)
	}
	u, err := url.Parse(call.Argument(0).String())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fail("%q is not an http or https URL", call.Argument(0).String())
	}
	if u.User != nil {
		return fail("credentials in the URL are not allowed")
	}
	if !r.hostAllowed(u.Hostname()) {
		return fail("host %q is not allowed (add it to PAYMUX_JS_HOOKS_ALLOWED_HOSTS)", u.Hostname())
	}
	method := "GET"
	var body string
	hasBody := false
	headers := http.Header{}
	if opts, ok := call.Argument(1).(*goja.Object); ok {
		if m := opts.Get("method"); m != nil && !goja.IsUndefined(m) && !goja.IsNull(m) {
			method = strings.ToUpper(m.String())
		}
		if bv := opts.Get("body"); bv != nil && !goja.IsUndefined(bv) && !goja.IsNull(bv) {
			body, hasBody = bv.String(), true
		}
		if hv, ok := opts.Get("headers").(*goja.Object); ok {
			for _, k := range hv.Keys() {
				switch strings.ToLower(k) {
				case "host", "content-length", "transfer-encoding", "connection":
					continue
				}
				headers.Set(k, hv.Get(k).String())
			}
		}
	}
	if !fetchMethods[method] {
		return fail("method %q is not supported", method)
	}
	if int64(len(body)) > r.o.MaxBody {
		return fail("request body too large (limit %d bytes)", r.o.MaxBody)
	}

	cs.b.pause() // waiting on the network is not script time
	defer cs.b.resume()
	ctx, cancel := context.WithTimeout(cs.ctx, r.o.FetchTimeout)
	defer cancel()
	var rd io.Reader
	if hasBody {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fail("%v", err)
	}
	req.Header = headers
	resp, err := r.o.HTTPClient.Do(req)
	if err != nil {
		return fail("%v", unwrapURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.o.MaxBody+1))
	if err != nil {
		return fail("reading the response: %v", err)
	}
	if int64(len(data)) > r.o.MaxBody {
		return fail("response too large (limit %d bytes)", r.o.MaxBody)
	}

	text := string(data)
	res := vm.NewObject()
	_ = res.Set("status", resp.StatusCode)
	_ = res.Set("ok", resp.StatusCode >= 200 && resp.StatusCode < 300)
	_ = res.Set("statusText", http.StatusText(resp.StatusCode))
	_ = res.Set("url", u.String())
	hdr := vm.NewObject()
	for k, v := range resp.Header {
		if len(v) > 0 {
			_ = hdr.Set(strings.ToLower(k), v[0])
		}
	}
	_ = res.Set("headers", hdr)
	_ = res.Set("text", func(goja.FunctionCall) goja.Value { return vm.ToValue(text) })
	_ = res.Set("json", func(goja.FunctionCall) goja.Value {
		v, err := in.jsonParse(goja.Undefined(), vm.ToValue(text))
		if err != nil {
			panic(vm.NewTypeError("fetch: the response is not valid JSON"))
		}
		return v
	})
	return res
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
