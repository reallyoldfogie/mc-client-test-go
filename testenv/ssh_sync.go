package testenv

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
)

// sshBaseArgs are the safety options every plain ssh invocation this
// package makes should use - never prompt (password, passphrase, a first-
// connection host-key question), so a broken or unreachable target fails
// fast as an error instead of hanging. Shared by startSSHTunnel and every
// function in this file.
func sshBaseArgs() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
	}
}

// runSSH runs `ssh <sshBaseArgs> target <remoteArgs...>` and returns a
// descriptive error (including the remote command's stderr) on failure.
func runSSH(ctx context.Context, target string, remoteArgs ...string) error {
	args := append(append([]string{}, sshBaseArgs()...), target)
	args = append(args, remoteArgs...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s %v: %w: %s", target, remoteArgs, err, stderr.String())
	}
	return nil
}

// syncDirToRemote copies the *contents* of localDir to remoteDir on target
// (creating remoteDir first), by streaming a tar archive over an SSH pipe:
// `tar -C localDir -cf - . | ssh target tar -C remoteDir -xf -`. Chosen
// over scp/rsync deliberately - both are reasonable alternatives, but tar
// piped through the same plain ssh this package already shells out to for
// everything else needs no second transfer protocol/binary and has no
// trailing-slash ambiguity (scp's "copy contents vs. copy the directory
// itself" behavior around a trailing "/." is a frequent source of subtly
// wrong transfers).
func syncDirToRemote(ctx context.Context, target, localDir, remoteDir string) error {
	if err := runSSH(ctx, target, "mkdir", "-p", remoteDir); err != nil {
		return fmt.Errorf("create remote directory %s: %w", remoteDir, err)
	}

	tarCmd := exec.CommandContext(ctx, "tar", "-C", localDir, "-cf", "-", ".")
	sshArgs := append(append([]string{}, sshBaseArgs()...), target, "tar", "-C", remoteDir, "-xf", "-")
	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)

	pipe, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sync %s: create pipe: %w", localDir, err)
	}
	sshCmd.Stdin = pipe

	var tarErr, sshErr bytes.Buffer
	tarCmd.Stderr = &tarErr
	sshCmd.Stderr = &sshErr

	if err := sshCmd.Start(); err != nil {
		return fmt.Errorf("sync %s: start remote tar: %w", localDir, err)
	}
	if err := tarCmd.Run(); err != nil {
		_ = sshCmd.Wait()
		return fmt.Errorf("sync %s: local tar: %w: %s", localDir, err, tarErr.String())
	}
	if err := sshCmd.Wait(); err != nil {
		return fmt.Errorf("sync %s: remote tar: %w: %s", localDir, err, sshErr.String())
	}
	return nil
}

// removeRemoteDir deletes dir (and everything in it) on target. Used to
// clean up a container's staged mod/config/data directories once it's
// removed - the same orphan-avoidance concern as closing its SSH tunnel.
func removeRemoteDir(ctx context.Context, target, dir string) error {
	if dir == "" {
		return nil
	}
	if err := runSSH(ctx, target, "rm", "-rf", dir); err != nil {
		return fmt.Errorf("remove remote directory %s: %w", dir, err)
	}
	return nil
}

// bindPlanEntry is one local directory ServerConfig wants bind-mounted,
// resolved to where it should land on the remote host (under some
// caller-chosen root) and inside the container.
type bindPlanEntry struct {
	localDir      string
	remoteDir     string
	containerDest string
}

// planLocalBinds is syncLocalBindsToRemote's pure half: given remoteRoot
// and cfg, it decides *where* each requested local directory
// (DataDir/ModsDir/ConfigDir/OutputDir/MountDirs) should be staged and what
// container path it ultimately binds to - without touching the filesystem,
// a network, or a subprocess. Separated out so this decision (especially
// MountDirs' basename handling, below) is unit-testable on its own; the
// actual transfer (syncDirToRemote) has no local/offline equivalent to test
// against, same as startSSHTunnel's own ssh dialing.
func planLocalBinds(remoteRoot string, cfg ServerConfig) []bindPlanEntry {
	var plan []bindPlanEntry
	add := func(localDir, remoteSubdir, containerDest string) {
		if localDir == "" {
			return
		}
		plan = append(plan, bindPlanEntry{
			localDir:      localDir,
			remoteDir:     path.Join(remoteRoot, remoteSubdir),
			containerDest: containerDest,
		})
	}

	add(cfg.DataDir, "data", "/data")
	add(cfg.ModsDir, "mods", "/data/mods")
	add(cfg.ConfigDir, "config", "/data/config")
	add(cfg.OutputDir, "output", "/data/output")
	for _, mount := range cfg.MountDirs {
		// A local-daemon bind uses mount as both the host source and (via
		// "/data/<mount>") the destination name, which only works because
		// it's assumed to already be a bare name, not a deeper path. The
		// remote-staged source path can't double as that name once it's
		// rooted under remoteRoot, so the destination name is taken from
		// mount's own base name instead - the same name a local-daemon
		// bind would produce for the common case this field is meant for
		// (a directory named, e.g., "resourcepacks").
		name := filepath.Base(mount)
		add(mount, "mount-"+name, "/data/"+name)
	}

	return plan
}

// syncLocalBindsToRemote stages every local directory bind ServerConfig
// requests under remoteRoot on target (per planLocalBinds), and returns the
// Docker bind strings (using the *remote* staged paths) Start should add to
// the container's HostConfig in their place.
//
// Returns as soon as any one sync fails, having already staged whichever
// directories came before it - the caller (Start) is responsible for
// removing remoteRoot on any error return, which cleans up a partial sync
// too, not just a fully-failed one.
func syncLocalBindsToRemote(ctx context.Context, target, remoteRoot string, cfg ServerConfig) ([]string, error) {
	var binds []string
	for _, entry := range planLocalBinds(remoteRoot, cfg) {
		if err := syncDirToRemote(ctx, target, entry.localDir, entry.remoteDir); err != nil {
			return nil, fmt.Errorf("sync %s: %w", entry.localDir, err)
		}
		binds = append(binds, fmt.Sprintf("%s:%s", entry.remoteDir, entry.containerDest))
	}
	return binds, nil
}
