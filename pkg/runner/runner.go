package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gptscript-ai/gptscript/pkg/config"
	"github.com/gptscript-ai/gptscript/pkg/credentials"
	"github.com/gptscript-ai/gptscript/pkg/engine"
	"github.com/gptscript-ai/gptscript/pkg/types"
	"golang.org/x/sync/errgroup"
)

type MonitorFactory interface {
	Start(ctx context.Context, prg *types.Program, env []string, input string) (Monitor, error)
}

type Monitor interface {
	Event(event Event)
	Stop(output string, err error)
}

type Options struct {
	MonitorFactory MonitorFactory        `usage:"-"`
	RuntimeManager engine.RuntimeManager `usage:"-"`
	StartPort      int64                 `usage:"-"`
	EndPort        int64                 `usage:"-"`
}

func complete(opts ...Options) (result Options) {
	for _, opt := range opts {
		result.MonitorFactory = types.FirstSet(opt.MonitorFactory, result.MonitorFactory)
		result.RuntimeManager = types.FirstSet(opt.RuntimeManager, result.RuntimeManager)
		result.StartPort = types.FirstSet(opt.StartPort, result.StartPort)
		result.EndPort = types.FirstSet(opt.EndPort, result.EndPort)
	}
	if result.MonitorFactory == nil {
		result.MonitorFactory = noopFactory{}
	}
	if result.EndPort == 0 {
		result.EndPort = result.StartPort
	}
	if result.StartPort == 0 {
		result.StartPort = result.EndPort
	}
	return
}

type Runner struct {
	c              engine.Model
	factory        MonitorFactory
	runtimeManager engine.RuntimeManager
	ports          engine.Ports
}

func New(client engine.Model, opts ...Options) (*Runner, error) {
	opt := complete(opts...)

	runner := &Runner{
		c:              client,
		factory:        opt.MonitorFactory,
		runtimeManager: opt.RuntimeManager,
	}

	if opt.StartPort != 0 {
		if opt.EndPort < opt.StartPort {
			return nil, fmt.Errorf("invalid port range: %d-%d", opt.StartPort, opt.EndPort)
		}
		runner.ports.SetPorts(opt.StartPort, opt.EndPort)
	}

	return runner, nil
}

func (r *Runner) Close() {
	r.ports.CloseDaemons()
}

func (r *Runner) Run(ctx context.Context, prg types.Program, env []string, input string) (output string, err error) {
	monitor, err := r.factory.Start(ctx, &prg, env, input)
	if err != nil {
		return "", err
	}
	defer func() {
		monitor.Stop(output, err)
	}()

	callCtx := engine.NewContext(ctx, &prg)
	return r.call(callCtx, monitor, env, input)
}

type Event struct {
	Time               time.Time              `json:"time,omitempty"`
	CallContext        *engine.Context        `json:"callContext,omitempty"`
	ToolSubCalls       map[string]engine.Call `json:"toolSubCalls,omitempty"`
	ToolResults        int                    `json:"toolResults,omitempty"`
	Type               EventType              `json:"type,omitempty"`
	ChatCompletionID   string                 `json:"chatCompletionId,omitempty"`
	ChatRequest        any                    `json:"chatRequest,omitempty"`
	ChatResponse       any                    `json:"chatResponse,omitempty"`
	ChatResponseCached bool                   `json:"chatResponseCached,omitempty"`
	Content            string                 `json:"content,omitempty"`
}

type EventType string

var (
	EventTypeCallStart    = EventType("callStart")
	EventTypeCallContinue = EventType("callContinue")
	EventTypeCallSubCalls = EventType("callSubCalls")
	EventTypeCallProgress = EventType("callProgress")
	EventTypeChat         = EventType("callChat")
	EventTypeCallFinish   = EventType("callFinish")
)

func (r *Runner) getContext(callCtx engine.Context, monitor Monitor, env []string) (result []engine.InputContext, _ error) {
	for _, contextToolName := range callCtx.Tool.Context {
		_, content, err := r.subCall(callCtx, monitor, env, contextToolName, "")
		if err != nil {
			return nil, err
		}
		result = append(result, engine.InputContext{
			ToolName: contextToolName,
			Content:  content,
		})
	}
	return result, nil
}

func (r *Runner) call(callCtx engine.Context, monitor Monitor, env []string, input string) (string, error) {
	progress, progressClose := streamProgress(&callCtx, monitor)
	defer progressClose()

	var err error

	if len(callCtx.Tool.Credentials) > 0 {
		env, err = r.handleCredentials(callCtx, monitor, env)
		if err != nil {
			return "", err
		}
	}

	callCtx.InputContext, err = r.getContext(callCtx, monitor, env)
	if err != nil {
		return "", err
	}

	e := engine.Engine{
		Model:          r.c,
		RuntimeManager: r.runtimeManager,
		Progress:       progress,
		Env:            env,
		Ports:          &r.ports,
	}

	monitor.Event(Event{
		Time:        time.Now(),
		CallContext: &callCtx,
		Type:        EventTypeCallStart,
		Content:     input,
	})

	result, err := e.Start(callCtx, input)
	if err != nil {
		return "", err
	}

	for {
		if result.Result != nil && len(result.Calls) == 0 {
			progressClose()
			monitor.Event(Event{
				Time:        time.Now(),
				CallContext: &callCtx,
				Type:        EventTypeCallFinish,
				Content:     *result.Result,
			})
			if err := recordStateMessage(result.State); err != nil {
				// Log a message if failed to record state message so that it doesn't affect the main process if state can't be recorded
				log.Infof("Failed to record state message: %v", err)
			}
			return *result.Result, nil
		}

		monitor.Event(Event{
			Time:         time.Now(),
			CallContext:  &callCtx,
			Type:         EventTypeCallSubCalls,
			ToolSubCalls: result.Calls,
		})

		callResults, err := r.subCalls(callCtx, monitor, env, result)
		if err != nil {
			return "", err
		}

		monitor.Event(Event{
			Time:        time.Now(),
			CallContext: &callCtx,
			Type:        EventTypeCallContinue,
			ToolResults: len(callResults),
		})

		result, err = e.Continue(callCtx.Ctx, result.State, callResults...)
		if err != nil {
			return "", err
		}
	}
}

func streamProgress(callCtx *engine.Context, monitor Monitor) (chan<- types.CompletionStatus, func()) {
	progress := make(chan types.CompletionStatus)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for status := range progress {
			if message := status.PartialResponse; message != nil {
				monitor.Event(Event{
					Time:             time.Now(),
					CallContext:      callCtx,
					Type:             EventTypeCallProgress,
					ChatCompletionID: status.CompletionID,
					Content:          message.String(),
				})
			} else {
				monitor.Event(Event{
					Time:               time.Now(),
					CallContext:        callCtx,
					Type:               EventTypeChat,
					ChatCompletionID:   status.CompletionID,
					ChatRequest:        status.Request,
					ChatResponse:       status.Response,
					ChatResponseCached: status.Cached,
				})
			}
		}
	}()

	var once sync.Once
	return progress, func() {
		once.Do(func() {
			close(progress)
			wg.Wait()
		})
	}
}

func (r *Runner) subCall(parentContext engine.Context, monitor Monitor, env []string, toolName, input string) (string, string, error) {
	toolID, ok := parentContext.Tool.ToolMapping[toolName]
	if !ok {
		return "", "", &types.ErrToolNotFound{
			ToolName: toolName,
		}
	}

	callCtx, err := parentContext.SubCall(parentContext.Ctx, toolID, "")
	if err != nil {
		return "", "", err
	}

	res, err := r.call(callCtx, monitor, env, input)
	return toolID, res, err
}

func (r *Runner) subCalls(callCtx engine.Context, monitor Monitor, env []string, lastReturn *engine.Return) (callResults []engine.CallResult, _ error) {
	var (
		resultLock sync.Mutex
	)

	eg, subCtx := errgroup.WithContext(callCtx.Ctx)
	for id, call := range lastReturn.Calls {
		eg.Go(func() error {
			callCtx, err := callCtx.SubCall(subCtx, call.ToolID, id)
			if err != nil {
				return err
			}

			result, err := r.call(callCtx, monitor, env, call.Input)
			if err != nil {
				return err
			}

			resultLock.Lock()
			defer resultLock.Unlock()
			callResults = append(callResults, engine.CallResult{
				ToolID: call.ToolID,
				CallID: id,
				Result: result,
			})

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return
}

// recordStateMessage record the final state of the openai request and fetch messages and tools for analysis purpose
// The name follows `gptscript-state-${hostname}-${unixtimestamp}`
func recordStateMessage(state *engine.State) error {
	if state == nil {
		return nil
	}
	tmpdir := os.TempDir()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	filename := filepath.Join(tmpdir, fmt.Sprintf("gptscript-state-%v-%v", hostname, time.Now().UnixMilli()))
	return os.WriteFile(filename, data, 0444)
}

func (r *Runner) handleCredentials(callCtx engine.Context, monitor Monitor, env []string) ([]string, error) {
	c, err := config.ReadCLIConfig("")
	if err != nil {
		return nil, fmt.Errorf("failed to read CLI config: %w", err)
	}

	store, err := credentials.NewStore(c)
	if err != nil {
		return nil, fmt.Errorf("failed to create credentials store: %w", err)
	}

	for _, credToolName := range callCtx.Tool.Credentials {
		cred, exists, err := store.Get(credToolName)
		if err != nil {
			return nil, fmt.Errorf("failed to get credentials for tool %s: %w", credToolName, err)
		}

		// If the credential doesn't already exist in the store, run the credential tool in order to get the value,
		// and save it in the store.
		if !exists {
			credToolID, res, err := r.subCall(callCtx, monitor, env, credToolName, "")
			if err != nil {
				return nil, fmt.Errorf("failed to run credential tool %s: %w", credToolName, err)
			}

			var envMap struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal([]byte(res), &envMap); err != nil {
				return nil, fmt.Errorf("failed to unmarshal credential tool %s response: %w", credToolName, err)
			}

			cred = &credentials.Credential{
				ToolID: credToolID,
				Env:    envMap.Env,
			}
			if err := store.Add(*cred); err != nil {
				return nil, fmt.Errorf("failed to add credential for tool %s: %w", credToolName, err)
			}
		}

		for k, v := range cred.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	return env, nil
}
