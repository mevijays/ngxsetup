package webui

import (
	"fmt"
	"net/http"

	"ngxsetup/internal/borg"
	"ngxsetup/internal/logx"
)

func (s *Server) handleBorgStatus(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := c.BorgStatus()
	resp := map[string]any{
		"configured": st.Configured,
		"repo":       st.Repo,
		"installed":  st.Installed,
		"reachable":  st.Reachable,
		"info":       st.Info,
		"schedule":   st.Schedule,
	}
	// Re-show the dedicated SSH key's public half (if one exists) so the
	// status panel can display/copy it without re-running setup — e.g.
	// after a page reload, or when the operator just wants to re-paste it
	// into ngxborg.
	if pub, err := c.BorgSSHPublicKey(); err == nil && pub != "" {
		resp["ssh_public_key"] = pub
		if st.Repo != "" {
			resp["ssh_key_suggested_command"] = borg.SuggestRemoteKeyCommand(st.Repo, pub)
		}
	}
	if st.StatsOK {
		resp["stats"] = map[string]any{
			"repo_id":                st.Stats.ID,
			"encryption":             st.Stats.Encryption,
			"last_modified":          st.Stats.LastModified,
			"total_size":             st.Stats.TotalSize,
			"total_compressed_size":  st.Stats.TotalCompressedSize,
			"unique_size":            st.Stats.UniqueSize,
			"unique_compressed_size": st.Stats.UniqueCompressedSize,
			"total_chunks":           st.Stats.TotalChunks,
			"unique_chunks":          st.Stats.UniqueChunks,
			"dedup_ratio":            st.Stats.DedupRatio(),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBorgSSHKeyGenerate generates (or, if one already exists, simply
// reuses and re-shows) the dedicated borg SSH key — independent of
// repository setup, so an operator can get a public key to register on a
// remote ngxborg server as its own first step. See
// provision.Ctx.BorgEnsureSSHKey's doc comment for why this never touches
// repository configuration.
func (s *Server) handleBorgSSHKeyGenerate(w http.ResponseWriter, r *http.Request) {
	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		pub, generated, err := c.BorgEnsureSSHKey()
		if err != nil {
			return err
		}
		if generated {
			logx.Change("generated a dedicated SSH key for borg")
		} else {
			logx.Info("reusing the existing dedicated SSH key for borg")
		}
		logx.Info("")
		logx.Info("ssh public key: %s", pub)
		respData := map[string]any{"public_key": pub, "generated": generated}
		// If a repository is already configured, offer the exact
		// registration command the same way the status panel and setup
		// flow do — nothing new to derive, just consistent with them.
		if st := c.BorgStatus(); st.Repo != "" {
			if hint := borg.SuggestRemoteKeyCommand(st.Repo, pub); hint != "" {
				respData["ssh_key_suggested_command"] = hint
			}
		}
		data = respData
		return nil
	})
	writeActionResult(w, output, err, data)
}

type borgSetupRequest struct {
	Repo          string `json:"repo"`
	Encryption    string `json:"encryption"`
	Compression   string `json:"compression"`
	Passphrase    string `json:"passphrase"`
	SSHPrivateKey string `json:"ssh_private_key"`
}

func (s *Server) handleBorgSetup(w http.ResponseWriter, r *http.Request) {
	var req borgSetupRequest
	if err := readJSON(r, &req); err != nil || req.Repo == "" {
		writeJSONError(w, http.StatusBadRequest, "a repository location is required")
		return
	}
	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		result, err := c.SetupBorgRepo(req.Repo, req.Encryption, req.Compression, req.Passphrase, req.SSHPrivateKey)
		// result can be non-nil even when err is not: SetupBorgRepo still
		// reports a freshly generated SSH key even if the `borg init`
		// that follows failed to reach the repository with it — the most
		// common way this fails on a first-ever setup against a remote
		// that does not know the new key yet. Surface that key in the
		// error response's data too, so the frontend can show it instead
		// of leaving the operator with a bare "Permission denied" and no
		// way to recover the key they need to register and retry with.
		if err != nil {
			if result != nil && result.PublicKey != "" {
				respData := map[string]any{"public_key": result.PublicKey, "generated_ssh_key": result.GeneratedSSHKey}
				if result.GeneratedSSHKey {
					if hint := borg.SuggestRemoteKeyCommand(req.Repo, result.PublicKey); hint != "" {
						respData["ssh_key_suggested_command"] = hint
					}
				}
				data = respData
			}
			return err
		}
		logx.Change("borg repository %s ready", req.Repo)
		if result.GeneratedPassphrase != "" {
			logx.Info("")
			logx.Info("passphrase: %s", result.GeneratedPassphrase)
			logx.Warn("this passphrase is shown once — write it down now. Without it, this repository's backups cannot be restored.")
		}
		respData := map[string]any{"generated_passphrase": result.GeneratedPassphrase}
		if result.PublicKey != "" {
			logx.Info("")
			logx.Info("ssh public key: %s", result.PublicKey)
			respData["public_key"] = result.PublicKey
			respData["generated_ssh_key"] = result.GeneratedSSHKey
			if result.GeneratedSSHKey {
				logx.Warn("this is a newly generated key — the remote server does not know it yet. Register it there before running a backup.")
				if hint := borg.SuggestRemoteKeyCommand(req.Repo, result.PublicKey); hint != "" {
					logx.Info("if the remote end runs ngxborg: %s", hint)
					respData["ssh_key_suggested_command"] = hint
				}
			}
		}
		data = respData
		return nil
	})
	writeActionResult(w, output, err, data)
}

type borgBackupRequest struct {
	Domain string `json:"domain"`
	Prune  bool   `json:"prune"`
}

func (s *Server) handleBorgBackup(w http.ResponseWriter, r *http.Request) {
	var req borgBackupRequest
	_ = readJSON(r, &req)

	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if req.Domain != "" {
			result, err := c.BorgBackupSite(req.Domain)
			if err != nil {
				return err
			}
			logx.Change("backed up %s to borg archive %s", result.Domain, result.Archive)
			data = result
		} else {
			results, err := c.BorgBackupAll()
			if err != nil {
				return err
			}
			failed := 0
			for _, res := range results {
				if res.Err != nil {
					logx.Error("%s: %v", res.Domain, res.Err)
					failed++
					continue
				}
				logx.Change("%s -> %s", res.Domain, res.Archive)
			}
			data = results
			if failed > 0 {
				return fmt.Errorf("%d borg backup(s) failed — see above", failed)
			}
		}
		if req.Prune {
			if err := c.BorgPrune(); err != nil {
				return fmt.Errorf("backup succeeded, but pruning failed: %w", err)
			}
		}
		return nil
	})
	writeActionResult(w, output, err, data)
}

// handleBorgArchives lists every archive together with its size and
// deduplication contribution — internal/borg.Client.Stats gathers both the
// archive list and the repository-wide summary in one round trip, so this
// endpoint returns both rather than making the frontend fetch them
// separately.
func (s *Server) handleBorgArchives(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	repoStats, archives, err := c.BorgArchiveDetails()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(archives))
	for _, a := range archives {
		out = append(out, map[string]any{
			"name":              a.Name,
			"time":              a.Time,
			"original_size":     a.OriginalSize,
			"compressed_size":   a.CompressedSize,
			"deduplicated_size": a.DeduplicatedSize,
			"nfiles":            a.NFiles,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"archives": out,
		"repo_stats": map[string]any{
			"total_size":             repoStats.TotalSize,
			"total_compressed_size":  repoStats.TotalCompressedSize,
			"unique_size":            repoStats.UniqueSize,
			"unique_compressed_size": repoStats.UniqueCompressedSize,
			"total_chunks":           repoStats.TotalChunks,
			"unique_chunks":          repoStats.UniqueChunks,
			"dedup_ratio":            repoStats.DedupRatio(),
		},
	})
}

type borgDeleteArchiveRequest struct {
	Archive string `json:"archive"`
}

// handleBorgDeleteArchive removes one archive from the repository —
// permanently and immediately, unlike the local-file DELETE /api/backups
// endpoint's os.Remove, this shells out to `borg delete` since an archive is
// not a file this process can unlink; borg itself owns freeing that space
// the next time the repository is compacted.
func (s *Server) handleBorgDeleteArchive(w http.ResponseWriter, r *http.Request) {
	var req borgDeleteArchiveRequest
	if err := readJSON(r, &req); err != nil || req.Archive == "" {
		writeJSONError(w, http.StatusBadRequest, "an archive name is required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		// BorgDeleteArchive -> borg.Client.DeleteArchive already logs the
		// change (see internal/borg/borg.go) — no need to say it twice.
		return c.BorgDeleteArchive(req.Archive)
	})
	writeActionResult(w, output, err, nil)
}

type borgRestoreRequest struct {
	Domain         string `json:"domain"`
	Archive        string `json:"archive"`
	Database       bool   `json:"database"`
	Files          bool   `json:"files"`
	ConfirmDomain  string `json:"confirm_domain"`
	NoSafetyBackup bool   `json:"no_safety_backup"`
}

func (s *Server) handleBorgRestore(w http.ResponseWriter, r *http.Request) {
	var req borgRestoreRequest
	if err := readJSON(r, &req); err != nil || req.Domain == "" || req.Archive == "" {
		writeJSONError(w, http.StatusBadRequest, "domain and archive are required")
		return
	}
	if !req.Database && !req.Files {
		writeJSONError(w, http.StatusBadRequest, "specify database, files, or both")
		return
	}
	if !constantTimeEqual(req.ConfirmDomain, req.Domain) {
		writeJSONError(w, http.StatusBadRequest, "confirm_domain must exactly match the domain being restored")
		return
	}

	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if req.Database {
			if _, err := c.BorgRestoreDatabase(req.Archive, req.Domain, req.NoSafetyBackup); err != nil {
				return err
			}
			logx.Change("database restored from %s", req.Archive)
		}
		if req.Files {
			if err := c.BorgRestoreFiles(req.Archive, req.Domain); err != nil {
				return err
			}
			logx.Change("files restored from %s", req.Archive)
		}
		return nil
	})
	writeActionResult(w, output, err, nil)
}

type borgScheduleRequest struct {
	OnCalendar string `json:"on_calendar"`
	Prune      bool   `json:"prune"`
	Disable    bool   `json:"disable"`
}

func (s *Server) handleBorgSchedule(w http.ResponseWriter, r *http.Request) {
	var req borgScheduleRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Disable && req.OnCalendar == "" {
		writeJSONError(w, http.StatusBadRequest, "on_calendar is required unless disable is set")
		return
	}

	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if req.Disable {
			if err := c.BorgRemoveSchedule(); err != nil {
				return err
			}
			logx.Change("scheduled borg backup removed")
			return nil
		}
		if err := c.BorgInstallSchedule(req.OnCalendar, req.Prune); err != nil {
			return err
		}
		logx.Change("scheduled borg backup installed (%s)", req.OnCalendar)
		return nil
	})
	writeActionResult(w, output, err, nil)
}
