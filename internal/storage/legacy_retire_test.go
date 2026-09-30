package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// State files written by the earlier defective build recorded a retired
// formula with state "retired" but without retired_at. Loading such a file
// must restore the committed retirement instead of reverting to approved.
func TestOpenRepairsLegacyRetirementWithoutTimestamp(t *testing.T) {
	dataDir := t.TempDir()
	updated := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	approvedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	type legacyFormula struct {
		ID                    string               `json:"id"`
		Name                  string               `json:"name"`
		Style                 string               `json:"style"`
		TargetOriginalGravity float64              `json:"target_original_gravity"`
		TargetFinalGravity    float64              `json:"target_final_gravity"`
		MaxFermentationTempC  float64              `json:"max_fermentation_temp_c"`
		MinimumDays           int                  `json:"minimum_days"`
		MaximumDays           int                  `json:"maximum_days"`
		Steps                 []domain.FormulaStep `json:"steps"`
		State                 domain.FormulaState  `json:"state"`
		Version               int64                `json:"version"`
		CreatedAt             time.Time            `json:"created_at"`
		UpdatedAt             time.Time            `json:"updated_at"`
		ApprovedAt            *time.Time           `json:"approved_at,omitempty"`
	}
	retiredID := "formula_retired_legacy"
	approvedID := "formula_approved"
	steps := []domain.FormulaStep{
		{Sequence: 1, Label: "Pitch", TemperatureC: 20, HoldMinutes: 60},
	}
	legacy := struct {
		Revision    int64                             `json:"revision"`
		UpdatedAt   time.Time                         `json:"updated_at"`
		Formulas    map[string]legacyFormula          `json:"formulas"`
		Vessels     map[string]domain.Vessel          `json:"vessels"`
		Runs        map[string]domain.FermentationRun `json:"runs"`
		NameIndex   map[string]string                 `json:"formula_name_index"`
		VesselIndex map[string]string                 `json:"vessel_code_index"`
		RunIndex    map[string]string                 `json:"run_code_index"`
	}{
		Revision: 3,
		Formulas: map[string]legacyFormula{
			retiredID: {
				ID: retiredID, Name: "Legacy Stout", Style: "Stout",
				TargetOriginalGravity: 1.060, TargetFinalGravity: 1.012,
				MaxFermentationTempC: 24, MinimumDays: 7, MaximumDays: 21,
				Steps: steps, State: domain.FormulaRetired, Version: 3,
				CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
				ApprovedAt: &approvedAt,
			},
			approvedID: {
				ID: approvedID, Name: "Legacy IPA", Style: "IPA",
				TargetOriginalGravity: 1.055, TargetFinalGravity: 1.010,
				MaxFermentationTempC: 22, MinimumDays: 5, MaximumDays: 14,
				Steps: steps, State: domain.FormulaApproved, Version: 2,
				CreatedAt: approvedAt, UpdatedAt: approvedAt, ApprovedAt: &approvedAt,
			},
		},
		Vessels:     map[string]domain.Vessel{},
		Runs:        map[string]domain.FermentationRun{},
		NameIndex:   map[string]string{"legacy stout": retiredID, "legacy ipa": approvedID},
		VesselIndex: map[string]string{},
		RunIndex:    map[string]string{},
	}
	raw, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, stateFileName), raw, 0o644); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	store, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open legacy state: %v", err)
	}
	if err := store.View(func(snapshot domain.Snapshot) error {
		retired := snapshot.Formulas[retiredID]
		if !retired.HasDurableRetirement() {
			t.Fatalf("legacy retired formula not restored: state=%q retired_at=%v",
				retired.State, retired.RetiredAt)
		}
		if retired.RetiredAt == nil || !retired.RetiredAt.Equal(updated) {
			t.Fatalf("retired_at should be backfilled from updated_at, got %v", retired.RetiredAt)
		}
		approved := snapshot.Formulas[approvedID]
		if approved.State != domain.FormulaApproved || approved.RetiredAt != nil {
			t.Fatalf("approved formula altered by migration: state=%q retired_at=%v",
				approved.State, approved.RetiredAt)
		}
		return nil
	}); err != nil {
		t.Fatalf("view legacy snapshot: %v", err)
	}

	// The repaired record must be persisted on the next committed write so
	// that subsequent restarts keep observing the retired state.
	if err := store.Execute(func(snapshot *domain.Snapshot) error { return nil }); err != nil {
		t.Fatalf("noop execute: %v", err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.View(func(snapshot domain.Snapshot) error {
		if !snapshot.Formulas[retiredID].HasDurableRetirement() {
			t.Fatalf("repaired retirement did not survive a second restart")
		}
		return nil
	}); err != nil {
		t.Fatalf("view reopened snapshot: %v", err)
	}
}
