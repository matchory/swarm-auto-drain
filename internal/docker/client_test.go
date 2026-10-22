package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	dockerclient "github.com/docker/docker/client"
)

// fakeDaemon serves /nodes and /tasks for one node, applying the
// `desired-state` task filter the way dockerd does.
func fakeDaemon(t *testing.T, tasks []swarm.Task) *dockerClient {
	t.Helper()
	node := swarm.Node{ID: "n1", Description: swarm.NodeDescription{Hostname: "whale-7"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.47/nodes", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]swarm.Node{node})
	})
	mux.HandleFunc("GET /v1.47/tasks", func(w http.ResponseWriter, r *http.Request) {
		args, err := filters.FromJSON(r.URL.Query().Get("filters"))
		if err != nil {
			t.Errorf("filters: %v", err)
		}
		out := []swarm.Task{}
		for i := range tasks {
			task := &tasks[i]
			if task.NodeID != args.Get("node")[0] {
				continue
			}
			if args.Contains("desired-state") && !args.ExactMatch("desired-state", string(task.DesiredState)) {
				continue
			}
			out = append(out, *task)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("tcp://"+srv.Listener.Addr().String()),
		dockerclient.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	return &dockerClient{c: cli}
}

func task(desired, actual swarm.TaskState) swarm.Task {
	return swarm.Task{NodeID: "n1", DesiredState: desired, Status: swarm.TaskStatus{State: actual}}
}

func TestNodeTaskCountCountsTasksStillRunningAfterDrain(t *testing.T) {
	// A drain flips every task's desired state to shutdown at once; the
	// containers keep running until the agent stops them.
	d := fakeDaemon(t, []swarm.Task{
		task(swarm.TaskStateShutdown, swarm.TaskStateRunning),
		task(swarm.TaskStateShutdown, swarm.TaskStateStarting),
		task(swarm.TaskStateShutdown, swarm.TaskStateShutdown),
		task(swarm.TaskStateShutdown, swarm.TaskStateFailed),
		task(swarm.TaskStateShutdown, swarm.TaskStateComplete),
	})
	got, err := d.NodeTaskCount(context.Background(), "whale-7")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("NodeTaskCount = %d, want 2 (running + starting)", got)
	}
}

func TestNodeTaskCountZeroWhenEveryTaskStopped(t *testing.T) {
	d := fakeDaemon(t, []swarm.Task{
		task(swarm.TaskStateShutdown, swarm.TaskStateShutdown),
		task(swarm.TaskStateRemove, swarm.TaskStateRejected),
		task(swarm.TaskStateShutdown, swarm.TaskStateOrphaned),
	})
	got, err := d.NodeTaskCount(context.Background(), "whale-7")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("NodeTaskCount = %d, want 0", got)
	}
}
