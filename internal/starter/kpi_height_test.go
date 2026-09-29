package starter

import "testing"

// A titled metric_kpi tile renders at 161 px and a KPI's height is only a
// minimum, so a shorter one tells the dashboard designer a height the tile
// does not have — and the row below is stacked against that wrong height.
// Every guide's KPI is kpiHeight tall, and the next row starts below it.
func TestKPITilesAreTallEnough(t *testing.T) {
	const renderedTitledKPI = 161
	if kpiHeight < renderedTitledKPI {
		t.Fatalf("kpiHeight = %d, below the %d px a titled KPI tile renders at", kpiHeight, renderedTitledKPI)
	}
	kpis := 0
	for _, pkg := range Packages() {
		for _, d := range pkg.Dashboards {
			for i, w := range d.Widgets {
				if w.WidgetType != "metric_kpi" {
					continue
				}
				kpis++
				if w.SizeH == nil || *w.SizeH != kpiHeight {
					got := "unset"
					if w.SizeH != nil {
						got = itoa(*w.SizeH)
					}
					t.Errorf("%s / %q: KPI %d is %s px tall, want kpiHeight (%d)", pkg.ModelName, d.Name, i, got, kpiHeight)
					continue
				}
				if w.PosY == nil {
					t.Errorf("%s / %q: KPI %d has no position", pkg.ModelName, d.Name, i)
					continue
				}
				bottom := *w.PosY + *w.SizeH
				for _, o := range d.Widgets {
					if o.PosY != nil && *o.PosY > *w.PosY && *o.PosY < bottom {
						t.Errorf("%s / %q: a %s widget starts at y=%d, inside KPI %d (y %d..%d)", pkg.ModelName, d.Name, o.WidgetType, *o.PosY, i, *w.PosY, bottom)
					}
				}
			}
		}
	}
	if kpis == 0 {
		t.Fatal("no metric_kpi widget in any guide")
	}
}
