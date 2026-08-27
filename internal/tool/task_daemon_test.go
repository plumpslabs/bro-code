package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTaskDaemonLifecycle(t *testing.T) {
	daemon := &TaskDaemon{
		tasks: make(map[string]*BackgroundTask),
	}
	spawnTool := &SpawnTaskTool{daemon: daemon}
	statusTool := &TaskStatusTool{daemon: daemon}
	logsTool := &TaskLogsTool{daemon: daemon}
	killTool := &KillTaskTool{daemon: daemon}

	// 1. Spawn a task that sleeps then prints
	res, err := spawnTool.Execute(context.Background(), `{"command":"echo 'hello background task' && sleep 1"}`)
	if err != nil {
		t.Fatalf("spawn_task failed: %v", err)
	}
	if !strings.Contains(res, "task_1") {
		t.Fatalf("expected task_1 in result, got: %s", res)
	}

	// 2. Check task status
	status, err := statusTool.Execute(context.Background(), `{"task_id":"task_1"}`)
	if err != nil {
		t.Fatalf("task_status failed: %v", err)
	}
	if !strings.Contains(status, "Task task_1") {
		t.Fatalf("expected status for task_1, got: %s", status)
	}

	// 3. Wait for output and check logs
	time.Sleep(200 * time.Millisecond)
	logs, err := logsTool.Execute(context.Background(), `{"task_id":"task_1","lines":10}`)
	if err != nil {
		t.Fatalf("task_logs failed: %v", err)
	}
	if !strings.Contains(logs, "hello background task") {
		t.Fatalf("expected log output, got: %s", logs)
	}

	// 4. Test list all tasks
	allTasks, err := statusTool.Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("task_status (all) failed: %v", err)
	}
	if !strings.Contains(allTasks, "Active Background Tasks") {
		t.Fatalf("expected task listing, got: %s", allTasks)
	}

	// 5. Test killing task
	_, _ = killTool.Execute(context.Background(), `{"task_id":"task_1"}`)
}
