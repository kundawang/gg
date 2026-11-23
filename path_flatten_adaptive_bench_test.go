package gg

import "testing"

// Benchmarks for the budget-constrained adaptive flattener. These cover the
// requirement-4 fixtures: thin ellipse, cusp, continuous cubics and the
// million-point cap, against the legacy Flatten.

func BenchmarkFlattenAdaptive_ThinEllipse(b *testing.B) {
	p := thinEllipsePath()
	opts := FlattenOptions{Tolerance: 0.1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {})
	}
}

func BenchmarkFlattenLegacy_ThinEllipse(b *testing.B) {
	p := thinEllipsePath()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Flatten(0.1)
	}
}

func BenchmarkFlattenAdaptive_Cusp(b *testing.B) {
	p := cuspPath()
	opts := FlattenOptions{Tolerance: 0.1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {})
	}
}

func BenchmarkFlattenAdaptive_ContinuousCubics(b *testing.B) {
	p := continuousCubicsPath(100)
	opts := FlattenOptions{Tolerance: 0.25}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {})
	}
}

func BenchmarkFlattenLegacy_ContinuousCubics(b *testing.B) {
	p := continuousCubicsPath(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Flatten(0.25)
	}
}

func BenchmarkFlattenAdaptive_RotateScale(b *testing.B) {
	p := continuousCubicsPath(60)
	m := Rotate(0.7).Multiply(Scale(2.2, 0.4))
	opts := FlattenOptions{Tolerance: 0.1, Transform: &m}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {})
	}
}

// BenchmarkFlattenAdaptive_BudgetCapped measures the relaxation path under a
// tight budget: repeated capped count drives plus the final emit.
func BenchmarkFlattenAdaptive_BudgetCapped(b *testing.B) {
	p := circlePath(400)
	opts := FlattenOptions{Tolerance: 1e-9, MaxPoints: 1024}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptiveCallback(opts, func(ev FlattenEvent, pt Point) {})
	}
}

func BenchmarkFlattenAdaptive_ReusedState(b *testing.B) {
	p := continuousCubicsPath(100)
	state := NewFlattenState()
	opts := FlattenOptions{Tolerance: 0.25}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.FlattenAdaptive(state, opts)
	}
}
