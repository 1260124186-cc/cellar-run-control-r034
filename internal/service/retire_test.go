package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func validFormula(name string) CreateFormulaInput {
	return CreateFormulaInput{
		Name:                  name,
		Style:                 "Stout",
		TargetOriginalGravity: 1.060,
		TargetFinalGravity:    1.012,
		MaxFermentationTempC:  24,
		MinimumDays:           7,
		MaximumDays:           21,
	}
}

func validSteps() []domain.FormulaStep {
	return []domain.FormulaStep{
		{Sequence: 1, Label: "Pitch", TemperatureC: 20, HoldMinutes: 1440},
	}
}

func createApprovedFormula(t *testing.T, app *Service, name string) domain.Formula {
	t.Helper()
	formula, err := app.CreateFormula(validFormula(name))
	if err != nil {
		t.Fatalf("create formula: %v", err)
	}
	formula, err = app.SetFormulaSteps(formula.ID,
		SetFormulaStepsInput{Steps: validSteps()})
	if err != nil {
		t.Fatalf("set steps: %v", err)
	}
	formula, err = app.ApproveFormula(formula.ID,
		VersionInput{ExpectedVersion: &formula.Version})
	if err != nil {
		t.Fatalf("approve formula: %v", err)
	}
	return formula
}

func createPlannedRun(t *testing.T, app *Service, formulaID, code string) domain.FermentationRun {
	t.Helper()
	run, err := app.CreateRun(CreateRunInput{
		Code:           code,
		FormulaID:      formulaID,
		PlannedVolumeL: 10,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func newTestService(t *testing.T) (*Service, *storage.Store, string, *clock.Fixed) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	fixed := clock.NewFixed(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	return New(store, fixed), store, dataDir, fixed
}

func reopenStore(t *testing.T, dataDir string, fixed *clock.Fixed) *Service {
	t.Helper()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	return New(store, fixed)
}

// A retirement that fails because an open run still references the formula
// must not poison later run creation against the still-approved formula.
func TestRetireFailureLeavesApprovedFormulaAdmissible(t *testing.T) {
	app, _, _, _ := newTestService(t)
	formula := createApprovedFormula(t, app, "Midnight Stout")
	openRun := createPlannedRun(t, app, formula.ID, "RUN-OPEN-1")

	_, err := app.RetireFormula(formula.ID, VersionInput{ExpectedVersion: nil})
	if !domain.IsCode(err, domain.CodeFormulaInUse) {
		t.Fatalf("expected formula_in_use conflict, got %v", err)
	}

	// The failed retirement leaves no side effects: the formula still reads
	// as approved and a legitimate new run must be accepted.
	loaded, err := app.GetFormula(formula.ID)
	if err != nil {
		t.Fatalf("get formula: %v", err)
	}
	if loaded.State != domain.FormulaApproved || loaded.RetiredAt != nil {
		t.Fatalf("formula should remain approved after failed retirement, got state=%q retired_at=%v",
			loaded.State, loaded.RetiredAt)
	}
	secondRun, err := app.CreateRun(CreateRunInput{
		Code: "RUN-OPEN-2", FormulaID: formula.ID, PlannedVolumeL: 10,
	})
	if err != nil {
		t.Fatalf("legitimate run creation after failed retirement was rejected: %v", err)
	}

	// Aborting the open runs frees the formula and retirement then succeeds.
	if _, _, err := app.AbortRun(openRun.ID, VersionInput{ExpectedVersion: nil}); err != nil {
		t.Fatalf("abort run: %v", err)
	}
	if _, _, err := app.AbortRun(secondRun.ID, VersionInput{ExpectedVersion: nil}); err != nil {
		t.Fatalf("abort second run: %v", err)
	}
	retired, err := app.RetireFormula(formula.ID, VersionInput{ExpectedVersion: nil})
	if err != nil {
		t.Fatalf("retire after abort: %v", err)
	}
	if retired.State != domain.FormulaRetired || retired.RetiredAt == nil {
		t.Fatalf("expected durably retired formula, got state=%q retired_at=%v",
			retired.State, retired.RetiredAt)
	}
}

// A successful retirement must survive a process restart and keep blocking
// new runs, while the historical run remains readable against the formula.
func TestSuccessfulRetirementPersistsAcrossRestart(t *testing.T) {
	app, _, dataDir, fixed := newTestService(t)
	formula := createApprovedFormula(t, app, "Harbor Porter")
	run := createPlannedRun(t, app, formula.ID, "RUN-DONE-1")
	if _, _, err := app.AbortRun(run.ID, VersionInput{ExpectedVersion: nil}); err != nil {
		t.Fatalf("abort run: %v", err)
	}
	retired, err := app.RetireFormula(formula.ID, VersionInput{ExpectedVersion: nil})
	if err != nil {
		t.Fatalf("retire formula: %v", err)
	}

	restarted := reopenStore(t, dataDir, fixed)
	loaded, err := restarted.GetFormula(formula.ID)
	if err != nil {
		t.Fatalf("get formula after restart: %v", err)
	}
	if loaded.State != domain.FormulaRetired {
		t.Fatalf("retirement did not survive restart, state=%q", loaded.State)
	}
	if loaded.RetiredAt == nil || !loaded.RetiredAt.Equal(*retired.RetiredAt) {
		t.Fatalf("retired_at not preserved across restart: got %v want %v",
			loaded.RetiredAt, retired.RetiredAt)
	}

	// New runs stay blocked after restart.
	_, err = restarted.CreateRun(CreateRunInput{
		Code: "RUN-AFTER-RESTART", FormulaID: formula.ID, PlannedVolumeL: 10,
	})
	if !domain.IsCode(err, domain.CodeFormulaState) {
		t.Fatalf("expected invalid_formula_state after restart, got %v", err)
	}

	// Historical runs and their original formula remain readable.
	historicalRun, err := restarted.GetRun(run.ID)
	if err != nil {
		t.Fatalf("read historical run: %v", err)
	}
	if historicalRun.State != domain.RunAborted || historicalRun.FormulaID != formula.ID {
		t.Fatalf("historical run altered: %+v", historicalRun)
	}
}

// The normal approval and run creation workflow is unchanged.
func TestDraftFormulaRejectsRunsAndApprovalAdmitsThem(t *testing.T) {
	app, _, _, _ := newTestService(t)
	created, err := app.CreateFormula(validFormula("Riverbend IPA"))
	if err != nil {
		t.Fatalf("create formula: %v", err)
	}
	if _, err := app.CreateRun(CreateRunInput{
		Code: "RUN-DRAFT", FormulaID: created.ID, PlannedVolumeL: 10,
	}); !domain.IsCode(err, domain.CodeFormulaState) {
		t.Fatalf("expected invalid_formula_state for draft formula, got %v", err)
	}
	created, err = app.SetFormulaSteps(created.ID,
		SetFormulaStepsInput{Steps: validSteps()})
	if err != nil {
		t.Fatalf("set steps: %v", err)
	}
	formula, err := app.ApproveFormula(created.ID,
		VersionInput{ExpectedVersion: &created.Version})
	if err != nil {
		t.Fatalf("approve formula: %v", err)
	}
	run := createPlannedRun(t, app, formula.ID, "RUN-LEGIT-1")
	if run.State != domain.RunPlanned {
		t.Fatalf("unexpected run state %q", run.State)
	}
}
