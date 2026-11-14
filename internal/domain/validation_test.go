package domain

import (
	"errors"
	"testing"
)

func TestValidateOccurrenceBounds(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	dur := i64(90000)

	// Unknown bounds (nil) vs zero (0) must both be accepted when valid
	if err := ValidateOccurrenceBounds(nil, nil, dur); err != nil {
		t.Fatalf("unknown bounds should be valid: %v", err)
	}
	if err := ValidateOccurrenceBounds(i64(0), i64(12000), dur); err != nil {
		t.Fatalf("zero start should be valid: %v", err)
	}
	if err := ValidateOccurrenceBounds(i64(-1), i64(5000), dur); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative start must fail validation")
	}
	if err := ValidateOccurrenceBounds(i64(20000), i64(20000), dur); !errors.Is(err, ErrValidation) {
		t.Fatalf("end == start must fail validation")
	}
	if err := ValidateOccurrenceBounds(i64(10000), i64(95000), dur); !errors.Is(err, ErrValidation) {
		t.Fatalf("end exceeding known duration must fail validation")
	}
}

func TestPortableMoveTokenAndDashLabels(t *testing.T) {
	tok := PortableMoveToken("Dile que no", "")
	if tok != "DileQueNo" {
		t.Fatalf("got %q, want DileQueNo", tok)
	}
	accented := PortableMoveToken("Setenta y complicación", "")
	if accented != "SetentaYComplicacion" {
		t.Fatalf("got %q, want SetentaYComplicacion", accented)
	}

	// Default: dash label preserved, no underscore companion unless explicitly requested
	tagOnly := BuildMoveTag("CubanSocial", "Enchufla doble", "", false)
	if tagOnly.DisplayLabel != "#CubanSocial-EnchuflaDoble" {
		t.Fatalf("got display label %q", tagOnly.DisplayLabel)
	}
	if tagOnly.PlatformHashtag != "" {
		t.Fatalf("companion hashtag must be empty when not opted in, got %q", tagOnly.PlatformHashtag)
	}

	// Explicit opt-in companion
	withComp := BuildMoveTag("CubanSocial", "Enchufla doble", "", true)
	if withComp.DisplayLabel != "#CubanSocial-EnchuflaDoble" || withComp.PlatformHashtag != "#CubanSocial_EnchuflaDoble" {
		t.Fatalf("unexpected companion pair: %+v", withComp)
	}
}

func TestDetectCombinationCycle(t *testing.T) {
	adj := map[string][]string{
		"combo-a": {"move-1", "combo-b"},
		"combo-b": {"move-2"},
	}
	if err := DetectCombinationCycle("combo-c", []string{"combo-a", "move-3"}, adj); err != nil {
		t.Fatalf("valid acyclic combination rejected: %v", err)
	}
	if err := DetectCombinationCycle("combo-b", []string{"combo-b"}, adj); !errors.Is(err, ErrValidation) {
		t.Fatalf("self-reference must be rejected")
	}
	// combo-b -> combo-c -> combo-a -> combo-b transitive cycle
	adj["combo-c"] = []string{"combo-a"}
	if err := DetectCombinationCycle("combo-b", []string{"combo-c"}, adj); !errors.Is(err, ErrValidation) {
		t.Fatalf("transitive cycle must be rejected")
	}
}
