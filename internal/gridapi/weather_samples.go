package gridapi

import (
	"math"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// figure accumulates one measured value over a point's readings. Only the
// readings that carried the value are added, so n is how many did.
type figure struct {
	n             int
	sum, min, max float64
}

func (f *figure) add(v float64) {
	if f.n == 0 {
		f.min, f.max = v, v
	}
	f.min, f.max = math.Min(f.min, v), math.Max(f.max, v)
	f.sum += v
	f.n++
}

func (f *figure) addPtr(v *float64) {
	if v != nil {
		f.add(*v)
	}
}

// roundDecimals rounds v to the given number of decimals, half away from
// zero (timeline.go's roundTo is for positive values only).
func roundDecimals(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

// mean, lowest and highest are nil when no reading carried the value: the
// wire's null, never a 0 nobody measured.
func (f *figure) mean(decimals int) *wrapperspb.DoubleValue {
	if f.n == 0 {
		return nil
	}
	return wrapperspb.Double(roundDecimals(f.sum/float64(f.n), decimals))
}

func (f *figure) lowest(decimals int) *wrapperspb.DoubleValue {
	if f.n == 0 {
		return nil
	}
	return wrapperspb.Double(roundDecimals(f.min, decimals))
}

func (f *figure) highest(decimals int) *wrapperspb.DoubleValue {
	if f.n == 0 {
		return nil
	}
	return wrapperspb.Double(roundDecimals(f.max, decimals))
}

// weatherCondition counts one OpenWeatherMap condition among a point's
// readings.
type weatherCondition struct {
	main, description string
	n                 int32
	last              time.Time
}

// weatherBucket accumulates the readings of one point.
type weatherBucket struct {
	at                                                   time.Time
	n                                                    int32
	temp, feels, humidity, wind, gust, visKm, rain, snow figure
	// The readings' wind as a vector sum (east and north components of where
	// it blows FROM, weighted by speed), for a direction that averages 350
	// and 10 degrees to north and not to south.
	windEast, windNorth float64
	windDirs            int
	conditions          []*weatherCondition
}

func (b *weatherBucket) add(s store.WeatherSample) {
	b.n++
	b.temp.add(s.TemperatureCelsius)
	b.feels.add(s.FeelsLikeCelsius)
	b.humidity.add(float64(s.HumidityPercent))
	b.wind.add(s.WindSpeedKmh)
	b.gust.addPtr(s.WindGustKmh)
	if s.VisibilityMeters != nil {
		b.visKm.add(float64(*s.VisibilityMeters) / 1000)
	}
	b.rain.addPtr(s.RainMmPerHour)
	b.snow.addPtr(s.SnowMmPerHour)
	// Calm has no direction, whatever the response put in the field.
	if s.WindDirectionDegrees != nil && s.WindSpeedKmh > 0 {
		rad := float64(*s.WindDirectionDegrees) * math.Pi / 180
		b.windEast += s.WindSpeedKmh * math.Sin(rad)
		b.windNorth += s.WindSpeedKmh * math.Cos(rad)
		b.windDirs++
	}
	if s.WeatherMain == "" && s.WeatherDescription == "" {
		return
	}
	for _, c := range b.conditions {
		if c.main == s.WeatherMain && c.description == s.WeatherDescription {
			c.n, c.last = c.n+1, s.At
			return
		}
	}
	b.conditions = append(b.conditions, &weatherCondition{main: s.WeatherMain, description: s.WeatherDescription, n: 1, last: s.At})
}

// windDirection is the direction of the readings' mean wind vector, 0 to
// 359, or nil when no reading had a direction with wind, or the vectors
// cancel (opposite winds of equal strength have no mean direction).
func (b *weatherBucket) windDirection() *wrapperspb.Int32Value {
	if b.windDirs == 0 || math.Hypot(b.windEast, b.windNorth) < 1e-6 {
		return nil
	}
	deg := int32(math.Round(math.Atan2(b.windEast, b.windNorth) * 180 / math.Pi))
	return wrapperspb.Int32(((deg % 360) + 360) % 360)
}

func (b *weatherBucket) point() *gridv1.WeatherSamplePoint {
	p := &gridv1.WeatherSamplePoint{
		At:                    timestamppb.New(b.at),
		Samples:               b.n,
		TemperatureCelsius:    b.temp.mean(2),
		TemperatureCelsiusMin: b.temp.lowest(2),
		TemperatureCelsiusMax: b.temp.highest(2),
		FeelsLikeCelsius:      b.feels.mean(2),
		FeelsLikeCelsiusMin:   b.feels.lowest(2),
		HumidityPercent:       b.humidity.mean(1),
		WindSpeedKmh:          b.wind.mean(1),
		WindSpeedKmhMax:       b.wind.highest(1),
		WindGustKmhMax:        b.gust.highest(1),
		WindDirectionDegrees:  b.windDirection(),
		VisibilityKm:          b.visKm.mean(2),
		VisibilityKmMin:       b.visKm.lowest(2),
		RainMmPerHour:         b.rain.mean(2),
		RainMmPerHourMax:      b.rain.highest(2),
		SnowMmPerHour:         b.snow.mean(2),
		SnowMmPerHourMax:      b.snow.highest(2),
	}
	// Most frequent first; among equals the one seen latest, then by name so
	// the order never depends on the rows' order.
	sort.SliceStable(b.conditions, func(i, j int) bool {
		x, y := b.conditions[i], b.conditions[j]
		if x.n != y.n {
			return x.n > y.n
		}
		if !x.last.Equal(y.last) {
			return x.last.After(y.last)
		}
		if x.main != y.main {
			return x.main < y.main
		}
		return x.description < y.description
	})
	for _, c := range b.conditions {
		p.Conditions = append(p.Conditions, &gridv1.WeatherSampleCondition{
			WeatherMain: c.main, WeatherDescription: c.description, Samples: c.n,
		})
	}
	return p
}

// weatherSamplePoints turns samples (oldest first) into points: one each when
// width is 0, else one per non-empty bucket of width seconds, aligned to the
// epoch and stamped with the bucket's start (as roadSamplePoints).
func weatherSamplePoints(samples []store.WeatherSample, width int64) []*gridv1.WeatherSamplePoint {
	var out []*gridv1.WeatherSamplePoint
	var cur *weatherBucket
	for _, s := range samples {
		at := s.At
		if width > 0 {
			at = time.Unix(floorDiv(s.At.Unix(), width)*width, 0).UTC()
		}
		if cur == nil || width == 0 || !cur.at.Equal(at) {
			if cur != nil {
				out = append(out, cur.point())
			}
			cur = &weatherBucket{at: at}
		}
		cur.add(s)
	}
	if cur != nil {
		out = append(out, cur.point())
	}
	return out
}
