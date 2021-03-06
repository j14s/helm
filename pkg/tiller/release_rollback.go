/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tiller

import (
	"fmt"
	ctx "golang.org/x/net/context"
	"k8s.io/helm/pkg/hooks"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/proto/hapi/services"
	"k8s.io/helm/pkg/timeconv"
)

// RollbackRelease rolls back to a previous version of the given release.
func (s *ReleaseServer) RollbackRelease(c ctx.Context, req *services.RollbackReleaseRequest) (*services.RollbackReleaseResponse, error) {
	err := s.env.Releases.LockRelease(req.Name)
	if err != nil {
		return nil, err
	}
	defer s.env.Releases.UnlockRelease(req.Name)

	currentRelease, targetRelease, err := s.prepareRollback(req)
	if err != nil {
		return nil, err
	}

	res, err := s.performRollback(currentRelease, targetRelease, req)
	if err != nil {
		return res, err
	}

	if !req.DryRun {
		if err := s.env.Releases.Create(targetRelease); err != nil {
			return res, err
		}
	}

	return res, nil
}

// prepareRollback finds the previous release and prepares a new release object with
//  the previous release's configuration
func (s *ReleaseServer) prepareRollback(req *services.RollbackReleaseRequest) (*release.Release, *release.Release, error) {
	switch {
	case !ValidName.MatchString(req.Name):
		return nil, nil, errMissingRelease
	case req.Version < 0:
		return nil, nil, errInvalidRevision
	}

	crls, err := s.env.Releases.Last(req.Name)
	if err != nil {
		return nil, nil, err
	}

	rbv := req.Version
	if req.Version == 0 {
		rbv = crls.Version - 1
	}

	s.Log("rolling back %s (current: v%d, target: v%d)", req.Name, crls.Version, rbv)

	prls, err := s.env.Releases.Get(req.Name, rbv)
	if err != nil {
		return nil, nil, err
	}

	// Store a new release object with previous release's configuration
	target := &release.Release{
		Name:      req.Name,
		Namespace: crls.Namespace,
		Chart:     prls.Chart,
		Config:    prls.Config,
		Info: &release.Info{
			FirstDeployed: crls.Info.FirstDeployed,
			LastDeployed:  timeconv.Now(),
			Status: &release.Status{
				Code:  release.Status_UNKNOWN,
				Notes: prls.Info.Status.Notes,
			},
			// Because we lose the reference to rbv elsewhere, we set the
			// message here, and only override it later if we experience failure.
			Description: fmt.Sprintf("Rollback to %d", rbv),
		},
		Version:  crls.Version + 1,
		Manifest: prls.Manifest,
		Hooks:    prls.Hooks,
	}

	return crls, target, nil
}

func (s *ReleaseServer) performRollback(currentRelease, targetRelease *release.Release, req *services.RollbackReleaseRequest) (*services.RollbackReleaseResponse, error) {
	res := &services.RollbackReleaseResponse{Release: targetRelease}

	if req.DryRun {
		s.Log("Dry run for %s", targetRelease.Name)
		return res, nil
	}

	// pre-rollback hooks
	if !req.DisableHooks {
		if err := s.execHook(targetRelease.Hooks, targetRelease.Name, targetRelease.Namespace, hooks.PreRollback, req.Timeout); err != nil {
			return res, err
		}
	}

	if err := s.ReleaseModule.Rollback(currentRelease, targetRelease, req, s.env); err != nil {
		msg := fmt.Sprintf("Rollback %q failed: %s", targetRelease.Name, err)
		s.Log("warning: %s", msg)
		currentRelease.Info.Status.Code = release.Status_SUPERSEDED
		targetRelease.Info.Status.Code = release.Status_FAILED
		targetRelease.Info.Description = msg
		s.recordRelease(currentRelease, true)
		s.recordRelease(targetRelease, false)
		return res, err
	}

	// post-rollback hooks
	if !req.DisableHooks {
		if err := s.execHook(targetRelease.Hooks, targetRelease.Name, targetRelease.Namespace, hooks.PostRollback, req.Timeout); err != nil {
			return res, err
		}
	}

	currentRelease.Info.Status.Code = release.Status_SUPERSEDED
	s.recordRelease(currentRelease, true)

	targetRelease.Info.Status.Code = release.Status_DEPLOYED

	return res, nil
}
