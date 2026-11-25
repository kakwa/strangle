package handlers

import (
	"net/http"

	"github.com/kakwa/strangle/internal/db"
)

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
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks [get]
func (a *API) CollectionPlaybooks(w http.ResponseWriter, r *http.Request) {
	coll, ok := a.collectionScope(r)
	if !ok {
		writeError(w, http.StatusNotFound, unknownCollection(r.PathValue("org"), r.PathValue("collection")))
		return
	}
	t, _ := a.collectionIn(r.PathValue("org"), coll)
	runs, err := a.Store.ListRuns(r.Context(), 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	latest := map[string]db.RunRecord{}
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
	writeJSON(w, http.StatusOK, map[string][]playbookInfo{coll: infos})
}
