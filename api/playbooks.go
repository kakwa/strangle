package api

import (
	"net/http"
	"time"

	"github.com/kakwa/strangle/store"
	"github.com/labstack/echo/v5"
)

type playbookInfo struct {
	Name       string     `json:"name"`
	Playbook   string     `json:"playbook"`
	Inventory  string     `json:"inventory"`
	LastStatus string     `json:"last_status,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
}

// CollectionPlaybooks godoc
//
// @ID      get_orgs_org_collections_collection_playbooks
// @Summary Playbooks of this collection
// @Tags    playbooks
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {object} map[string][]playbookInfo "Playbooks keyed by collection"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection"
// @Failure 500 {object} apiError "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks [get]
func (a *API) CollectionPlaybooks(c *echo.Context) error {
	r := c.Request()
	coll, err := a.collectionScope(c)
	if err != nil {
		return err
	}
	t, _ := a.collectionIn(c.Param("org"), coll)
	runs, err := a.Store.ListRuns(r.Context(), 1000)
	if err != nil {
		return apiStatus(http.StatusInternalServerError, err)
	}
	latest := map[string]store.RunRecord{}
	for _, run := range runs {
		if run.Target != coll {
			continue
		}
		if cur, ok := latest[run.Playbook]; !ok || run.FinishedAt.After(cur.FinishedAt) {
			latest[run.Playbook] = run
		}
	}
	infos := playbookInfos(t.Playbooks)
	for i := range infos {
		if run, ok := latest[infos[i].Name]; ok {
			infos[i].LastStatus = map[bool]string{true: "ok", false: "failed"}[run.Success]
			at := run.FinishedAt
			infos[i].LastRunAt = &at
		}
	}
	return c.JSON(http.StatusOK, map[string][]playbookInfo{coll: infos})
}
