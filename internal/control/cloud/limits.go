package cloud

// baselineGbps is the network bandwidth an instance size can hold, in
// gigabits per second, for the sizes HourlyPrice knows. Sizes sold as
// "up to" a figure can burst above it for a while; this is the rate they
// keep, which is the line a busy server runs into.
var baselineGbps = map[string]float64{
	"t3.nano": 0.032, "t3.micro": 0.064, "t3.small": 0.128, "t3.medium": 0.256, "t3.large": 0.512,
	"t3.xlarge": 1.024, "t3.2xlarge": 2.048,
	"t4g.nano": 0.032, "t4g.micro": 0.064, "t4g.small": 0.128, "t4g.medium": 0.256, "t4g.large": 0.512,
	"t4g.xlarge": 1.024, "t4g.2xlarge": 2.048,
}

// largeGbps is the baseline of the m, c and r sizes, which share it by
// size within a generation.
var largeGbps = map[string]float64{"large": 0.781, "xlarge": 1.562, "2xlarge": 3.125, "4xlarge": 6.25}

// NetworkBaseline is a size's baseline network bandwidth in bytes per
// second, when it is known.
func NetworkBaseline(size string) (float64, bool) {
	if g, ok := baselineGbps[size]; ok {
		return g * 1e9 / 8, true
	}
	if _, ok := hourly[size]; !ok {
		return 0, false
	}
	for i := 0; i < len(size); i++ {
		if size[i] == '.' {
			if g, ok := largeGbps[size[i+1:]]; ok {
				return g * 1e9 / 8, true
			}
			break
		}
	}
	return 0, false
}
