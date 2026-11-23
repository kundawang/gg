package filter

import "math"

// GaussianKernel generates a 1D Gaussian kernel for the given radius.
// The kernel is normalized so all values sum to 1.0.
//
// The kernel size is computed as 2 * ceil(radius * 3) + 1, which covers
// 99.7% of the Gaussian distribution (3 standard deviations).
//
// For radius <= 0 or a non-finite radius (NaN, +/-Inf), returns a
// single-element kernel [1.0] (identity). A finite radius above
// maxGaussianSigma is clamped to maxGaussianSigma so memory use stays
// bounded regardless of caller input.
func GaussianKernel(radius float64) []float32 {
	if !isFinitePositive(radius) {
		return []float32{1}
	}
	if radius > maxGaussianSigma {
		radius = maxGaussianSigma
	}

	// Kernel size: 2 * ceil(sigma * 3) + 1
	// Using radius as sigma
	sigma := radius
	halfSize := int(math.Ceil(sigma * 3))
	size := halfSize*2 + 1

	kernel := make([]float32, size)

	// Gaussian formula: G(x) = exp(-x²/(2σ²)) / (σ√(2π))
	// We skip the normalization constant since we'll normalize sum to 1
	twoSigmaSq := 2 * sigma * sigma
	sum := float64(0)

	for i := 0; i < size; i++ {
		x := float64(i - halfSize)
		val := math.Exp(-(x * x) / twoSigmaSq)
		kernel[i] = float32(val)
		sum += val
	}

	// Normalize so kernel sums to 1.0
	if sum > 0 {
		invSum := float32(1.0 / sum)
		for i := range kernel {
			kernel[i] *= invSum
		}
	}

	return kernel
}

// BoxKernel generates a 1D box (uniform) kernel for the given radius.
// All values are equal: 1/(2*radius+1).
//
// Box blur is faster than Gaussian but produces blocky results.
// Three passes of box blur approximate Gaussian blur well.
func BoxKernel(radius int) []float32 {
	if radius <= 0 {
		return []float32{1}
	}

	size := radius*2 + 1
	kernel := make([]float32, size)
	val := float32(1.0) / float32(size)

	for i := range kernel {
		kernel[i] = val
	}

	return kernel
}

// OptimalKernelSize returns the optimal kernel size for a given radius.
// This is useful for pre-allocating buffers.
func OptimalKernelSize(radius float64) int {
	if radius <= 0 {
		return 1
	}
	halfSize := int(math.Ceil(radius * 3))
	return halfSize*2 + 1
}

// KernelCenter returns the center index of a kernel of the given size.
func KernelCenter(kernelSize int) int {
	return kernelSize / 2
}
