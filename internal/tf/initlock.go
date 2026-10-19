package tf

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// terraform init runs one at a time: roots are read in parallel, and
// terraform does not promise that a shared plugin cache (TF_PLUGIN_CACHE_DIR
// or plugin_cache_dir in the CLI config) survives concurrent installs.
// Terragrunt runs init on its own, so tforphans gives itself to terragrunt
// as its terraform (TG_TF_PATH); that wrapper asks this process for the
// turn over a unix socket before it runs init.
var initTurn = make(chan struct{}, 1)

// lockInit waits for the turn to run init. The wait does not count
// against the budget of ctx (see WithBudget), only ctx's cancel does.
func lockInit(ctx context.Context) (func(), error) {
	if b, ok := ctx.Value(budgetKey{}).(*budgetCtx); ok {
		defer b.pause()()
	}
	select {
	case initTurn <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-initTurn }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type budgetKey struct{}

// budgetCtx is cancelled when its time runs out, as context.WithTimeout,
// but the clock stops while the work waits for the init turn: a root that
// queues behind other inits has its whole -source-timeout once it runs.
type budgetCtx struct {
	context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	left    time.Duration
	since   time.Time
	timer   *time.Timer
	pauses  int
	expired bool
}

// WithBudget returns a context that times out after d of time not spent
// waiting for the init turn.
func WithBudget(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	b := &budgetCtx{Context: ctx, cancel: cancel, left: d, since: time.Now()}
	b.timer = time.AfterFunc(d, b.fire)
	return b, func() {
		b.mu.Lock()
		b.timer.Stop()
		b.mu.Unlock()
		cancel()
	}
}

func (b *budgetCtx) fire() {
	b.mu.Lock()
	if b.pauses > 0 {
		// Fired as a pause began: nothing is left after it.
		b.left = 0
		b.mu.Unlock()
		return
	}
	b.expired = true
	b.mu.Unlock()
	b.cancel()
}

func (b *budgetCtx) pause() (resume func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pauses++
	if b.pauses == 1 && !b.expired && b.timer.Stop() {
		b.left -= time.Since(b.since)
	}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.pauses--
		if b.pauses == 0 && !b.expired {
			b.since = time.Now()
			b.timer = time.AfterFunc(max(b.left, 0), b.fire)
		}
	}
}

func (b *budgetCtx) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expired {
		return context.DeadlineExceeded
	}
	return b.Context.Err()
}

func (b *budgetCtx) Value(key any) any {
	if key == (budgetKey{}) {
		return b
	}
	return b.Context.Value(key)
}

// The environment of the wrapper: the terraform it runs, the socket that
// gives out the turn, and the token of the waiting unit.
const (
	wrapEnvBin    = "TFORPHANS_WRAP_TERRAFORM"
	wrapEnvSocket = "TFORPHANS_INIT_SOCKET"
	wrapEnvToken  = "TFORPHANS_INIT_TOKEN"
)

// turnServer gives the init turn to wrappers; a turn is held while the
// connection is open, so a killed wrapper gives it back.
type turnServer struct {
	dir    string
	socket string
	mu     sync.Mutex
	waits  map[string]context.Context // token: context of the unit's read
}

var (
	serverOnce sync.Once
	server     *turnServer
	serverErr  error
)

func turns() (*turnServer, error) {
	serverOnce.Do(func() {
		// A directory of its own (0700): no one else can connect, and the
		// path stays short enough for a socket.
		dir, err := os.MkdirTemp("", "tforphans-")
		if err != nil {
			serverErr = err
			return
		}
		s := &turnServer{dir: dir, socket: filepath.Join(dir, "init.sock"), waits: map[string]context.Context{}}
		l, err := net.Listen("unix", s.socket)
		if err != nil {
			os.RemoveAll(dir)
			serverErr = fmt.Errorf("init turn socket: %w", err)
			return
		}
		go s.serve(l)
		server = s
	})
	return server, serverErr
}

// CloseTurns removes the socket of the init turns, if one was made.
func CloseTurns() {
	if server != nil {
		os.RemoveAll(server.dir)
	}
}

// register makes a token for a terragrunt run under ctx.
func (s *turnServer) register(ctx context.Context) (string, func()) {
	token := rand.Text()
	s.mu.Lock()
	s.waits[token] = ctx
	s.mu.Unlock()
	return token, func() {
		s.mu.Lock()
		delete(s.waits, token)
		s.mu.Unlock()
	}
}

func (s *turnServer) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *turnServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	s.mu.Lock()
	ctx, ok := s.waits[strings.TrimSpace(line)]
	s.mu.Unlock()
	if !ok {
		fmt.Fprintln(c, "unknown")
		return
	}
	unlock, err := lockInit(ctx)
	if err != nil {
		fmt.Fprintln(c, "cancelled")
		return
	}
	defer unlock()
	if _, err := fmt.Fprintln(c, "go"); err != nil {
		return
	}
	// Held until the wrapper closes the connection (init done, or the
	// wrapper is gone) or the unit's read is cancelled.
	closed := make(chan struct{})
	go func() {
		_, _ = r.ReadByte()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
	}
}

// wrapperEnv is the environment for terragrunt that makes tforphans its
// terraform: inits then wait for their turn under ctx.
func wrapperEnv(ctx context.Context, tfBin string) ([]string, func(), error) {
	s, err := turns()
	if err != nil {
		return nil, nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("tforphans as terragrunt's terraform: %w", err)
	}
	// Terragrunt runs terraform in its cache: a relative path would not
	// name it there.
	bin, err := exec.LookPath(tfBin)
	if err != nil {
		return nil, nil, err
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, nil, err
	}
	token, done := s.register(ctx)
	return []string{
		"TG_TF_PATH=" + self, "TERRAGRUNT_TFPATH=" + self,
		wrapEnvBin + "=" + bin, wrapEnvSocket + "=" + s.socket, wrapEnvToken + "=" + token,
	}, done, nil
}

// RunWrapper runs terraform for terragrunt and exits, when this process
// was started as the wrapper (see wrapperEnv); otherwise it returns.
func RunWrapper() {
	bin := os.Getenv(wrapEnvBin)
	if bin == "" {
		return
	}
	os.Exit(runWrapper(bin, os.Args[1:]))
}

func runWrapper(bin string, args []string) int {
	socket, token := os.Getenv(wrapEnvSocket), os.Getenv(wrapEnvToken)
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != wrapEnvBin && name != wrapEnvSocket && name != wrapEnvToken {
			env = append(env, kv)
		}
	}
	if subcommand(args) == "init" {
		c, err := waitTurn(socket, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tforphans: terraform init for terragrunt: %v\n", err)
			return 1
		}
		defer func() { _ = c.Close() }()
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, env
	// Signals go to terraform, which stops on its own; the wrapper waits
	// for it so that the turn is held until init is over.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, forwardedSignals...)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tforphans: %v\n", err)
		return 1
	}
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return exit.ExitCode()
	default:
		fmt.Fprintf(os.Stderr, "tforphans: %v\n", err)
		return 1
	}
}

// waitTurn asks the tforphans process that started terragrunt for the
// init turn; the turn lasts while the returned connection is open.
func waitTurn(socket, token string) (net.Conn, error) {
	if socket == "" || token == "" {
		return nil, errors.New("no init turn socket")
	}
	c, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(c, token); err != nil {
		_ = c.Close()
		return nil, err
	}
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || strings.TrimSpace(reply) != "go" {
		_ = c.Close()
		return nil, fmt.Errorf("no init turn (%s)", strings.TrimSpace(reply))
	}
	return c, nil
}

// subcommand is terraform's command: the first argument that is not a
// global option (-chdir=DIR and the like).
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}
