// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package taskrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	ti "github.com/hashicorp/nomad/client/allocrunner/taskrunner/interfaces"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// the name of this hook, used in logs
	sidsHookName = "consul_si_token"

	// sidsDerivationTimeout limits the amount of time we may spend trying to
	// derive a SI token. If the hook does not get a token within this amount of
	// time, the result is a failure.
	sidsDerivationTimeout = 5 * time.Minute

	// sidsTokenFile is the name of the file holding the Consul SI token inside
	// the task's secret directory
	sidsTokenFile = "si_token"

	// sidsTokenFilePerms is the level of file permissions granted on the file
	// in the secrets directory for the task
	sidsTokenFilePerms = 0440
)

type sidsHookConfig struct {
	alloc              *structs.Allocation
	task               *structs.Task
	lifecycle          ti.TaskLifecycle
	logger             hclog.Logger
	allocHookResources *cstructs.AllocHookResources
}

// Service Identities hook for managing SI tokens of connect enabled tasks.
type sidsHook struct {
	// alloc is the allocation
	alloc *structs.Allocation

	// taskName is the name of the task
	task *structs.Task

	// lifecycle is used to signal, restart, and kill a task
	lifecycle ti.TaskLifecycle

	// logger is used to log
	logger hclog.Logger

	// lock variables that can be manipulated after hook creation
	lock sync.Mutex
	// firstRun keeps track of whether the hook is being called for the first
	// time (for this task) during the lifespan of the Nomad Client process.
	firstRun bool

	// allocHookResources gives us access to Consul tokens that may have been
	// set by the consul_hook
	allocHookResources *cstructs.AllocHookResources
}

func newSIDSHook(c sidsHookConfig) *sidsHook {
	return &sidsHook{
		alloc:              c.alloc,
		task:               c.task,
		lifecycle:          c.lifecycle,
		logger:             c.logger.Named(sidsHookName),
		firstRun:           true,
		allocHookResources: c.allocHookResources,
	}
}

func (h *sidsHook) Name() string {
	return sidsHookName
}

func (h *sidsHook) Prestart(
	ctx context.Context,
	req *interfaces.TaskPrestartRequest,
	resp *interfaces.TaskPrestartResponse) error {

	h.lock.Lock()
	defer h.lock.Unlock()

	// do nothing if we have already done things
	if h.earlyExit() {
		resp.Done = true
		return nil
	}

	// if we're using Workload Identities then this Connect task should already
	// have a token stored under the cluster + service ID.
	tokens := h.allocHookResources.GetConsulTokens()

	// Find the group-level service that this task belongs to
	tg := h.alloc.Job.LookupTaskGroup(h.alloc.TaskGroup)
	serviceName := h.task.Kind.Value()
	var serviceIdentityName string
	var cluster string
	for _, service := range tg.Services {
		if service.Name == serviceName {
			serviceIdentityName = service.MakeUniqueIdentityName()
			cluster = service.GetConsulClusterName(tg)
			break
		}
	}
	if cluster != "" && serviceIdentityName != "" {
		if token, ok := tokens[cluster][serviceIdentityName]; ok {
			if err := h.writeToken(req.TaskDir.SecretsDir, token.SecretID); err != nil {
				return err
			}
			resp.Done = true
			return nil
		}
	}

	resp.Done = true
	return nil
}

// earlyExit returns true if the Prestart hook has already been executed during
// the instantiation of this task runner.
//
// assumes h is locked
func (h *sidsHook) earlyExit() bool {
	if h.firstRun {
		h.firstRun = false
		return false
	}
	return true
}

// writeToken writes token into the secrets directory for the task.
func (h *sidsHook) writeToken(dir string, token string) error {
	tokenPath := filepath.Join(dir, sidsTokenFile)
	if err := os.WriteFile(tokenPath, []byte(token), sidsTokenFilePerms); err != nil {
		return fmt.Errorf("failed to write SI token: %w", err)
	}
	return nil
}

// recoverToken returns the token saved to disk in the secrets directory for the
// task if it exists, or the empty string if the file does not exist. an error
// is returned only for some other (e.g. disk IO) error.
func (h *sidsHook) recoverToken(dir string) (string, error) {
	tokenPath := filepath.Join(dir, sidsTokenFile)
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			h.logger.Error("failed to recover SI token", "error", err)
			return "", fmt.Errorf("failed to recover SI token: %w", err)
		}
		h.logger.Trace("no pre-existing SI token to recover", "task", h.task.Name)
		return "", nil // token file does not exist yet
	}
	h.logger.Trace("recovered pre-existing SI token", "task", h.task.Name)
	return string(token), nil
}

func (h *sidsHook) kill(ctx context.Context, reason error) {
	if err := h.lifecycle.Kill(ctx,
		structs.NewTaskEvent(structs.TaskKilling).
			SetFailsTask().
			SetDisplayMessage(reason.Error()),
	); err != nil {
		h.logger.Error("failed to kill task", "kill_reason", reason, "error", err)
	}
}
