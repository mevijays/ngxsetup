package webui

import (
	"net/http"
	"strconv"

	"ngxsetup/internal/db"
	"ngxsetup/internal/logx"
)

// ---- databases --------------------------------------------------------------

func (s *Server) handleDBManageDatabases(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dbs, err := c.DBClient().ListDatabases(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, map[string]any{
			"name": d.Name, "size_mb": d.SizeMB, "tables": d.Tables, "collation": d.Collation,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- users --------------------------------------------------------------------

func (s *Server) handleDBManageUsersList(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	client := c.DBClient()
	users, err := client.ListUsers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		// Best-effort: an account with grants this build's SHOW GRANTS
		// parsing can't cope with (a legacy PASSWORD() column format, for
		// instance) still shows up in the list, just without its grants —
		// one account's server-version quirk should not hide every other
		// account from the list.
		grants, gerr := client.UserGrants(r.Context(), u.User, u.Host)
		if gerr != nil {
			grants = []string{"(could not read grants: " + gerr.Error() + ")"}
		}
		out = append(out, map[string]any{"user": u.User, "host": u.Host, "grants": grants})
	}
	writeJSON(w, http.StatusOK, out)
}

type dbManageCreateUserRequest struct {
	User     string `json:"user"`
	Host     string `json:"host"`
	Password string `json:"password"`
}

func (s *Server) handleDBManageUserCreate(w http.ResponseWriter, r *http.Request) {
	var req dbManageCreateUserRequest
	if err := readJSON(r, &req); err != nil || req.User == "" {
		writeJSONError(w, http.StatusBadRequest, "a username is required")
		return
	}
	if req.Host == "" {
		req.Host = "localhost"
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := c.DBClient().CreateManagedUser(r.Context(), req.User, req.Host, req.Password); err != nil {
			return err
		}
		logx.Change("created database user %s@%s", req.User, req.Host)
		return nil
	})
	writeActionResult(w, output, err, nil)
}

type dbManageUserRefRequest struct {
	User string `json:"user"`
	Host string `json:"host"`
}

func (s *Server) handleDBManageUserDrop(w http.ResponseWriter, r *http.Request) {
	var req dbManageUserRefRequest
	if err := readJSON(r, &req); err != nil || req.User == "" || req.Host == "" {
		writeJSONError(w, http.StatusBadRequest, "user and host are required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := c.DBClient().DropManagedUser(r.Context(), req.User, req.Host); err != nil {
			return err
		}
		logx.Change("dropped database user %s@%s", req.User, req.Host)
		return nil
	})
	writeActionResult(w, output, err, nil)
}

type dbManageGrantRequest struct {
	User       string   `json:"user"`
	Host       string   `json:"host"`
	Database   string   `json:"database"`
	Privileges []string `json:"privileges"`
}

func (s *Server) handleDBManageGrant(w http.ResponseWriter, r *http.Request) {
	var req dbManageGrantRequest
	if err := readJSON(r, &req); err != nil || req.User == "" || req.Host == "" || req.Database == "" {
		writeJSONError(w, http.StatusBadRequest, "user, host and database are required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := c.DBClient().GrantPrivileges(r.Context(), req.User, req.Host, req.Database, req.Privileges); err != nil {
			return err
		}
		logx.Change("granted %v on %s to %s@%s", req.Privileges, req.Database, req.User, req.Host)
		return nil
	})
	writeActionResult(w, output, err, nil)
}

func (s *Server) handleDBManageRevoke(w http.ResponseWriter, r *http.Request) {
	var req dbManageGrantRequest
	if err := readJSON(r, &req); err != nil || req.User == "" || req.Host == "" || req.Database == "" {
		writeJSONError(w, http.StatusBadRequest, "user, host and database are required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := c.DBClient().RevokeAllPrivileges(r.Context(), req.User, req.Host, req.Database); err != nil {
			return err
		}
		logx.Change("revoked all privileges on %s from %s@%s", req.Database, req.User, req.Host)
		return nil
	})
	writeActionResult(w, output, err, nil)
}

// ---- tables and rows ----------------------------------------------------------

func (s *Server) handleDBManageTables(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tables, err := c.DBClient().ListTables(r.Context(), r.PathValue("db"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(tables))
	for _, t := range tables {
		out = append(out, map[string]any{
			"name": t.Name, "engine": t.Engine, "rows": t.Rows, "size_mb": t.SizeMB, "has_primary_key": t.HasPrimary,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDBManageTableRows(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dbName, table := r.PathValue("db"), r.PathValue("table")
	client := c.DBClient()

	cols, err := client.TableColumns(r.Context(), dbName, table)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const pageSize = 100
	rows, total, err := client.TableRows(r.Context(), dbName, table, cols, page, pageSize)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	colsOut := make([]map[string]any, 0, len(cols))
	for _, col := range cols {
		colsOut = append(colsOut, map[string]any{
			"name": col.Name, "type": col.Type, "nullable": col.Nullable, "primary_key": col.PrimaryKey,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns":   colsOut,
		"rows":      rows,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"editable":  hasPrimaryKey(cols),
	})
}

type dbManageRowUpdateRequest struct {
	PrimaryKey map[string]string `json:"primary_key"`
	Changes    map[string]string `json:"changes"`
}

func (s *Server) handleDBManageRowUpdate(w http.ResponseWriter, r *http.Request) {
	var req dbManageRowUpdateRequest
	if err := readJSON(r, &req); err != nil || len(req.PrimaryKey) == 0 {
		writeJSONError(w, http.StatusBadRequest, "primary_key is required")
		return
	}
	dbName, table := r.PathValue("db"), r.PathValue("table")
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		client := c.DBClient()
		cols, err := client.TableColumns(r.Context(), dbName, table)
		if err != nil {
			return err
		}
		if err := client.UpdateRow(r.Context(), dbName, table, cols, req.PrimaryKey, req.Changes); err != nil {
			return err
		}
		logx.Change("updated a row in %s.%s", dbName, table)
		return nil
	})
	writeActionResult(w, output, err, nil)
}

func hasPrimaryKey(cols []db.ColumnInfo) bool {
	for _, c := range cols {
		if c.PrimaryKey {
			return true
		}
	}
	return false
}
