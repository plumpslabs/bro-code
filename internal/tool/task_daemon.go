package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// BackgroundTask represents a managed running or finished background process.
type BackgroundTask struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	PID       int       `json:"pid"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time,omitempty"`
	Running   bool      `json:"running"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`

	mu     sync.Mutex
	logs   []string
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func (t *BackgroundTask) appendLog(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logs = append(t.logs, line)
	if len(t.logs) > 1000 {
		t.logs = t.logs[len(t.logs)-1000:]
	}
}

func (t *BackgroundTask) getLogs(tail int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tail <= 0 || tail >= len(t.logs) {
		copied := make([]string, len(t.logs))
		copy(copied, t.logs)
		return copied
	}
	start := len(t.logs) - tail
	copied := make([]string, tail)
	copy(copied, t.logs[start:])
	return copied
}

// TaskDaemon manages spawned background processes.
type TaskDaemon struct {
	mu      sync.RWMutex
	tasks   map[string]*BackgroundTask
	workDir string
	nextID  int
}

var globalDaemon = &TaskDaemon{
	tasks: make(map[string]*BackgroundTask),
}

// GlobalTaskDaemon returns the singleton task manager.
func GlobalTaskDaemon() *TaskDaemon {
	return globalDaemon
}

func (d *TaskDaemon) SetWorkDir(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.workDir = dir
}

// Spawn starts a background process and immediately returns its task ID.
func (d *TaskDaemon) Spawn(ctx context.Context, command string) (*BackgroundTask, error) {
	d.mu.Lock()
	d.nextID++
	id := fmt.Sprintf("task_%d", d.nextID)
	d.mu.Unlock()

	taskCtx, cancel := context.WithCancel(context.Background())

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		if bashPath, err := exec.LookPath("bash"); err == nil {
			cmd = exec.CommandContext(taskCtx, bashPath, "-c", command)
		} else {
			cmd = exec.CommandContext(taskCtx, "cmd.exe", "/c", command)
		}
	} else {
		cmd = exec.CommandContext(taskCtx, "sh", "-c", command)
	}

	d.mu.RLock()
	if d.workDir != "" {
		cmd.Dir = d.workDir
	}
	d.mu.RUnlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start process: %w", err)
	}

	task := &BackgroundTask{
		ID:        id,
		Command:   command,
		PID:       cmd.Process.Pid,
		StartTime: time.Now(),
		Running:   true,
		cmd:       cmd,
		cancel:    cancel,
		logs:      make([]string, 0, 100),
	}

	d.mu.Lock()
	d.tasks[id] = task
	d.mu.Unlock()

	// Read stdout and stderr asynchronously with separate goroutines
	var logWg sync.WaitGroup
	logWg.Add(2)

	go func() {
		defer logWg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			task.appendLog(scanner.Text())
		}
	}()

	go func() {
		defer logWg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			task.appendLog(scanner.Text())
		}
	}()

	go func() {
		logWg.Wait()
		_ = cmd.Wait()
		task.mu.Lock()
		task.Running = false
		task.EndTime = time.Now()
		if cmd.ProcessState != nil {
			task.ExitCode = cmd.ProcessState.ExitCode()
		}
		task.mu.Unlock()
	}()

	return task, nil
}

// GetTask returns a task by ID.
func (d *TaskDaemon) GetTask(id string) (*BackgroundTask, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.tasks[id]
	return t, ok
}

// Kill stops a running background task.
func (d *TaskDaemon) Kill(id string) error {
	d.mu.RLock()
	task, ok := d.tasks[id]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	task.mu.Lock()
	if !task.Running {
		task.mu.Unlock()
		return fmt.Errorf("task %q is already finished", id)
	}
	if task.cancel != nil {
		task.cancel()
	}
	if task.cmd != nil && task.cmd.Process != nil {
		_ = task.cmd.Process.Kill()
	}
	task.Running = false
	task.EndTime = time.Now()
	task.mu.Unlock()
	task.appendLog("⚠️ [Task terminated by user/agent]")
	return nil
}

// ListTasks returns all tracked background tasks.
func (d *TaskDaemon) ListTasks() []*BackgroundTask {
	d.mu.RLock()
	defer d.mu.RUnlock()
	list := make([]*BackgroundTask, 0, len(d.tasks))
	for _, t := range d.tasks {
		list = append(list, t)
	}
	return list
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool Implementations: spawn_task, task_status, task_logs, kill_task
// ─────────────────────────────────────────────────────────────────────────────

// SpawnTaskTool spawns a long-running process in background.
type SpawnTaskTool struct {
	daemon *TaskDaemon
}

func (t *SpawnTaskTool) Name() string { return "spawn_task" }
func (t *SpawnTaskTool) Description() string {
	return "Spawn a long-running background command (e.g. dev server, file watcher, compiler). Returns a task_id immediately without blocking the agent."
}
func (t *SpawnTaskTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Shell command to run in background"},
		},
		"required": []string{"command"},
	}
}
func (t *SpawnTaskTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("command cannot be empty")
	}
	d := t.daemon
	if d == nil {
		d = GlobalTaskDaemon()
	}
	task, err := d.Spawn(ctx, args.Command)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("🚀 Background task spawned successfully!\nTask ID: %s\nPID: %d\nCommand: %s\nStatus: Running\n👉 Use task_logs(task_id=%q) to inspect stdout/stderr.", task.ID, task.PID, task.Command, task.ID), nil
}

// TaskStatusTool checks status of background tasks.
type TaskStatusTool struct {
	daemon *TaskDaemon
}

func (t *TaskStatusTool) Name() string { return "task_status" }
func (t *TaskStatusTool) Description() string {
	return "Inspect the current status, PID, runtime duration, and exit code of background tasks. Omit task_id to list all active tasks."
}
func (t *TaskStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "Optional task ID to inspect"},
		},
	}
}
func (t *TaskStatusTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	d := t.daemon
	if d == nil {
		d = GlobalTaskDaemon()
	}

	if args.TaskID != "" {
		task, ok := d.GetTask(args.TaskID)
		if !ok {
			return "", fmt.Errorf("task %q not found", args.TaskID)
		}
		status := "🟢 Running"
		duration := time.Since(task.StartTime).Round(time.Second)
		if !task.Running {
			status = fmt.Sprintf("⚪ Finished (Exit Code: %d)", task.ExitCode)
			duration = task.EndTime.Sub(task.StartTime).Round(time.Second)
		}
		return fmt.Sprintf("Task %s:\n• Command: %s\n• PID: %d\n• Status: %s\n• Duration: %s", task.ID, task.Command, task.PID, status, duration), nil
	}

	tasks := d.ListTasks()
	if len(tasks) == 0 {
		return "No background tasks currently active.", nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Active Background Tasks (%d):\n", len(tasks)))
	for _, task := range tasks {
		status := "🟢 Running"
		if !task.Running {
			status = fmt.Sprintf("⚪ Finished (%d)", task.ExitCode)
		}
		cmdSnippet := task.Command
		if len(cmdSnippet) > 40 {
			cmdSnippet = cmdSnippet[:37] + "..."
		}
		sb.WriteString(fmt.Sprintf("- [%s] PID %d | %s | %s\n", task.ID, task.PID, status, cmdSnippet))
	}
	return sb.String(), nil
}

// TaskLogsTool inspects stdout/stderr logs from a background task.
type TaskLogsTool struct {
	daemon *TaskDaemon
}

func (t *TaskLogsTool) Name() string { return "task_logs" }
func (t *TaskLogsTool) Description() string {
	return "Retrieve recent stdout and stderr output logs from a background task."
}
func (t *TaskLogsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "Task ID returned by spawn_task"},
			"lines":   map[string]any{"type": "integer", "description": "Number of recent lines to tail (default: 50)"},
		},
		"required": []string{"task_id"},
	}
}
func (t *TaskLogsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Lines  int    `json:"lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	if args.Lines <= 0 {
		args.Lines = 50
	}
	d := t.daemon
	if d == nil {
		d = GlobalTaskDaemon()
	}
	task, ok := d.GetTask(args.TaskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", args.TaskID)
	}
	logs := task.getLogs(args.Lines)
	if len(logs) == 0 {
		return fmt.Sprintf("Task %s has not emitted any logs yet.", task.ID), nil
	}
	return fmt.Sprintf("=== Logs for %s (last %d lines) ===\n%s", task.ID, len(logs), strings.Join(logs, "\n")), nil
}

// KillTaskTool terminates a background task.
type KillTaskTool struct {
	daemon *TaskDaemon
}

func (t *KillTaskTool) Name() string { return "kill_task" }
func (t *KillTaskTool) Description() string {
	return "Gracefully terminate a running background task process by its task_id."
}
func (t *KillTaskTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "Task ID to terminate"},
		},
		"required": []string{"task_id"},
	}
}
func (t *KillTaskTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	d := t.daemon
	if d == nil {
		d = GlobalTaskDaemon()
	}
	if err := d.Kill(args.TaskID); err != nil {
		return "", err
	}
	return fmt.Sprintf("🛑 Successfully terminated task %s.", args.TaskID), nil
}
