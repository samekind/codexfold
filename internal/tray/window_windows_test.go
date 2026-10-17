//go:build windows

package tray

import "testing"

func TestInitialWindowFitsWorkArea(t *testing.T) {
	for _, work := range []rect{{0, 0, 1920, 1040}, {0, 0, 1366, 728}, {-1280, 0, 0, 984}} {
		for _, scale := range []float64{1, 1.25, 1.5, 2, 3} {
			bounds := fitWindow(work, scale)
			if bounds.Left < work.Left || bounds.Top < work.Top || bounds.Right > work.Right || bounds.Bottom > work.Bottom || bounds.Left >= bounds.Right || bounds.Top >= bounds.Bottom {
				t.Fatalf("work=%+v scale=%v: window %+v does not fit", work, scale, bounds)
			}
		}
	}
}

func TestGlassFrameResizeOnNegativeMonitorCoordinates(t *testing.T) {
	bounds := rect{-1280, 100, -280, 880}
	for _, test := range []struct {
		cursor point
		want   uintptr
	}{
		{point{-1279, 101}, 13}, {point{-281, 101}, 14}, {point{-1279, 879}, 16}, {point{-281, 879}, 17},
		{point{-1279, 400}, 10}, {point{-281, 400}, 11}, {point{-800, 101}, 12}, {point{-800, 879}, 15}, {point{-800, 400}, 0},
	} {
		if got := frameResizeHit(bounds, test.cursor, 6); got != test.want {
			t.Fatalf("cursor=%+v got=%d want=%d", test.cursor, got, test.want)
		}
	}
}
