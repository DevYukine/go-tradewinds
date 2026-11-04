package store

import "testing"

// Same-basis pooled calibration (planner.md D1): a sparse
// basis+commodity+horizon cell must resolve to its own basis's broad pool
// (basis+horizon first, then basis-only, pooled minimum 30 samples) before any
// cross-basis commodity, group or global evidence is consulted. Retained
// books are too young for 30-sample specific cells; without the pools their
// trades silently inherit depth or global receipt factors.

func TestSparseRetainedCellUsesSameBasisHorizonPoolBeforeCrossBasisFallback(t *testing.T) {
	t.Parallel()
	model := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: map[string]RevenueCalibrationCell{
		"basis/retained/commodity/indian_calico/1.5-2.5": {Samples: 7, MedianBPS: 8027, AdverseBPS: 8027},
		"basis/retained/1.5-2.5":                         {Samples: 30, MedianBPS: 8200, AdverseBPS: 7800},
		"commodity/indian_calico/1.5-2.5":                {Samples: 40, MedianBPS: 9500, AdverseBPS: 9300},
		"group/textile":                                  {Samples: 60, MedianBPS: 9600, AdverseBPS: 9400},
		"global":                                         {Samples: 353, MedianBPS: 9539, AdverseBPS: 9114},
	}}
	got := model.Factor("indian_calico", "retained", 2)
	if got.Source != "basis/retained/1.5-2.5" || got.MedianBPS != 8200 || got.Samples != 30 {
		t.Fatalf("sparse retained cell skipped its own pooled horizon evidence and used %q (n=%d, %d bps): %+v",
			got.Source, got.Samples, got.MedianBPS, got)
	}
}

func TestSparseRetainedCellUsesBasisOnlyPoolWhenHorizonPoolIsSparse(t *testing.T) {
	t.Parallel()
	model := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: map[string]RevenueCalibrationCell{
		"basis/retained/commodity/indian_calico/1.5-2.5": {Samples: 7, MedianBPS: 8027, AdverseBPS: 8027},
		"basis/retained/1.5-2.5":                         {Samples: 29, MedianBPS: 8200, AdverseBPS: 7800},
		"basis/retained":                                 {Samples: 30, MedianBPS: 8100, AdverseBPS: 7700},
		"commodity/indian_calico/1.5-2.5":                {Samples: 40, MedianBPS: 9500, AdverseBPS: 9300},
		"global":                                         {Samples: 353, MedianBPS: 9539, AdverseBPS: 9114},
	}}
	got := model.Factor("indian_calico", "retained", 2)
	if got.Source != "basis/retained" || got.MedianBPS != 8100 || got.Samples != 30 {
		t.Fatalf("retained cell below the pooled horizon minimum skipped its basis-only pool and used %q (n=%d, %d bps): %+v",
			got.Source, got.Samples, got.MedianBPS, got)
	}
}

func TestSameBasisPoolNeedsThirtySamplesAndHorizonPoolPrecedesBasisOnly(t *testing.T) {
	t.Parallel()
	sparse := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: map[string]RevenueCalibrationCell{
		"basis/retained/1.5-2.5": {Samples: 29, MedianBPS: 8200, AdverseBPS: 7800},
		"basis/retained":         {Samples: 29, MedianBPS: 8100, AdverseBPS: 7700},
		"global":                 {Samples: 353, MedianBPS: 9539, AdverseBPS: 9114},
	}}
	if got := sparse.Factor("indian_calico", "retained", 2); got.Source != "global" {
		t.Fatalf("twenty-nine pooled samples were treated as measured basis evidence: %+v", got)
	}
	usable := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: map[string]RevenueCalibrationCell{
		"basis/retained/1.5-2.5": {Samples: 30, MedianBPS: 8200, AdverseBPS: 7800},
		"basis/retained":         {Samples: 30, MedianBPS: 8800, AdverseBPS: 8600},
	}}
	got := usable.Factor("indian_calico", "retained", 2)
	if got.Source != "basis/retained/1.5-2.5" || got.MedianBPS != 8200 {
		t.Fatalf("thirty pooled samples did not unlock the horizon pool ahead of the basis-only pool: %+v", got)
	}
}

func TestDepthBasisUsesSameBasisPoolBeforeCrossBasisFallback(t *testing.T) {
	t.Parallel()
	model := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: map[string]RevenueCalibrationCell{
		"basis/depth/commodity/canvas_sails/2.5+": {Samples: 20, MedianBPS: 9100, AdverseBPS: 8900},
		"basis/depth/2.5+":                        {Samples: 30, MedianBPS: 9000, AdverseBPS: 8800},
		"commodity/canvas_sails/2.5+":             {Samples: 50, MedianBPS: 9700, AdverseBPS: 9500},
		"global":                                  {Samples: 353, MedianBPS: 9539, AdverseBPS: 9114},
	}}
	got := model.Factor("canvas_sails", "depth", 3)
	if got.Source != "basis/depth/2.5+" || got.MedianBPS != 9000 {
		t.Fatalf("sparse depth cell skipped its own pooled horizon evidence and used %q (%d bps): %+v",
			got.Source, got.MedianBPS, got)
	}
}
