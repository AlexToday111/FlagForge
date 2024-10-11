package api

import (
	"github.com/flagforge/flagforge/internal/repository"
	"github.com/flagforge/flagforge/pkg/types"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	out, err := s.Repo.CreateProject(r.Context(), principal(r), input.Key, input.Name)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.Projects(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var e types.Environment
	if !decode(w, r, &e) {
		return
	}
	e, key, err := s.Repo.CreateEnvironment(r.Context(), principal(r), r.PathValue("project"), e)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, map[string]any{"environment": e, "sdk_key": key})
}
func (s *Server) environments(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.Environments(r.Context(), r.PathValue("project"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) createFlag(w http.ResponseWriter, r *http.Request) {
	var f types.FeatureFlag
	if !decode(w, r, &f) {
		return
	}
	out, err := s.Repo.CreateFlag(r.Context(), principal(r), r.PathValue("project"), f)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) flags(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.Flags(r.Context(), r.PathValue("project"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) flag(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.Flags(r.Context(), r.PathValue("project"))
	if err != nil {
		failure(w, err)
		return
	}
	for _, f := range out {
		if f.Key == r.PathValue("flag") {
			write(w, 200, f)
			return
		}
	}
	failure(w, repository.ErrNotFound)
}
func (s *Server) patchFlag(w http.ResponseWriter, r *http.Request) {
	var patch map[string]string
	if !decode(w, r, &patch) {
		return
	}
	out, err := s.Repo.UpdateFlag(r.Context(), principal(r), r.PathValue("project"), r.PathValue("flag"), patch)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) archiveFlag(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.UpdateFlag(r.Context(), principal(r), r.PathValue("project"), r.PathValue("flag"), map[string]string{"status": "ARCHIVED"})
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	f, err := s.Repo.Config(r.Context(), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"))
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(f.Version, 10)+`"`)
	write(w, 200, f)
}
func (s *Server) update(w http.ResponseWriter, r *http.Request, version int64, f types.FlagConfig) {
	out, err := s.Repo.UpdateConfig(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"), version, f)
	if err != nil {
		failure(w, err)
		return
	}
	s.Metrics.Updates.Inc()
	w.Header().Set("ETag", `"`+strconv.FormatInt(out.Version, 10)+`"`)
	write(w, 200, out)
}
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	var f types.FlagConfig
	if !decode(w, r, &f) {
		return
	}
	s.update(w, r, version, f)
}
func (s *Server) toggle(w http.ResponseWriter, r *http.Request, on bool) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	f, err := s.Repo.Config(r.Context(), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"))
	if err != nil {
		failure(w, err)
		return
	}
	f.Enabled = on
	s.update(w, r, version, f)
}
func (s *Server) enable(w http.ResponseWriter, r *http.Request)  { s.toggle(w, r, true) }
func (s *Server) disable(w http.ResponseWriter, r *http.Request) { s.toggle(w, r, false) }
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	old, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		failure(w, repository.ErrInvalid)
		return
	}
	f, err := s.Repo.History(r.Context(), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"), old)
	if err != nil {
		failure(w, err)
		return
	}
	s.update(w, r, version, f)
}
func (s *Server) adminSnapshot(w http.ResponseWriter, r *http.Request) {
	envs, err := s.Repo.Environments(r.Context(), r.PathValue("project"))
	if err != nil {
		failure(w, err)
		return
	}
	for _, e := range envs {
		if e.Key == r.PathValue("env") {
			out, err := s.Repo.Snapshot(r.Context(), e.ID)
			if err != nil {
				failure(w, err)
				return
			}
			write(w, 200, out)
			return
		}
	}
	failure(w, repository.ErrNotFound)
}
func (s *Server) putSegment(w http.ResponseWriter, r *http.Request) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	var seg types.Segment
	if !decode(w, r, &seg) {
		return
	}
	seg.Key = r.PathValue("segment")
	v, err := s.Repo.PutSegment(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), version, seg)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, map[string]int64{"version": v})
}
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.SDKKeys(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	id, key, err := s.Repo.RotateSDKKey(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, map[string]string{"id": id, "sdk_key": key})
}
func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Repo.RevokeSDKKey(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), r.PathValue("id")); err != nil {
		failure(w, err)
		return
	}
	write(w, 204, nil)
}
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor string `json:"actor"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &input) {
		return
	}
	id, key, err := s.Repo.CreateToken(r.Context(), principal(r), input.Actor, input.Role)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, map[string]string{"id": id, "token": key})
}
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Repo.RevokeToken(r.Context(), principal(r), r.PathValue("id")); err != nil {
		failure(w, err)
		return
	}
	write(w, 204, nil)
}
func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	var input struct {
		RunAt  time.Time        `json:"run_at"`
		Config types.FlagConfig `json:"config"`
	}
	if !decode(w, r, &input) {
		return
	}
	out, err := s.Repo.Schedule(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"), version, input.RunAt, input.Config)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) gradual(w http.ResponseWriter, r *http.Request) {
	version, ok := expected(w, r)
	if !ok {
		return
	}
	var input struct {
		Variation string `json:"variation"`
		Stages    []struct {
			Percentage int       `json:"percentage"`
			StartAt    time.Time `json:"start_at"`
		} `json:"stages"`
	}
	if !decode(w, r, &input) {
		return
	}
	f, err := s.Repo.Config(r.Context(), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"))
	if err != nil {
		failure(w, err)
		return
	}
	if _, ok := f.Variations[input.Variation]; !ok || input.Variation == f.OffVariation {
		failure(w, repository.ErrInvalid)
		return
	}
	stages := []repository.ScheduledChange{}
	last := -1
	for _, st := range input.Stages {
		if st.Percentage < 0 || st.Percentage > 100 || st.Percentage < last {
			failure(w, repository.ErrInvalid)
			return
		}
		last = st.Percentage
		stage := f
		stage.Enabled = true
		stage.Rollout = []types.RolloutVariation{{VariationKey: input.Variation, Weight: st.Percentage * 1000}, {VariationKey: f.OffVariation, Weight: 100000 - st.Percentage*1000}}
		stages = append(stages, repository.ScheduledChange{RunAt: st.StartAt, Config: stage})
	}
	out, err := s.Repo.ScheduleStages(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), r.PathValue("flag"), version, stages)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	out, err := s.Repo.Schedules(r.Context(), r.PathValue("project"), r.PathValue("env"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.Repo.CancelSchedule(r.Context(), principal(r), r.PathValue("project"), r.PathValue("env"), r.PathValue("id")); err != nil {
		failure(w, err)
		return
	}
	write(w, 204, nil)
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	filters := map[string]string{}
	for _, k := range []string{"project", "environment", "flag", "actor", "action", "from", "to"} {
		filters[k] = r.URL.Query().Get(k)
	}
	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		var err error
		after, err = strconv.ParseInt(v, 10, 64)
		if err != nil || after < 0 {
			failure(w, repository.ErrInvalid)
			return
		}
	}
	out, err := s.Repo.Audit(r.Context(), filters, after)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) stale(w http.ResponseWriter, r *http.Request) { s.usageResponse(w, r, true) }
func (s *Server) usage(w http.ResponseWriter, r *http.Request) { s.usageResponse(w, r, false) }
func (s *Server) usageResponse(w http.ResponseWriter, r *http.Request, stale bool) {
	out, err := s.Repo.Usage(r.Context(), r.URL.Query().Get("project"), stale)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, out)
}
