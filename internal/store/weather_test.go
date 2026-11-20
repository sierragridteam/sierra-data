package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sample is stored as reported: decimals kept, and a field the response
// did not carry stays NULL (a dry hour is not a measured 0 mm).
func TestWeatherSamplesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	full := WeatherSample{
		LocationID: "bearvalley", At: t0,
		TemperatureCelsius: -0.62, FeelsLikeCelsius: -5.4, HumidityPercent: 93,
		WindSpeedKmh: 16.6, WindDirectionDegrees: i32(250), WindGustKmh: f64(41.8),
		VisibilityMeters: i32(800), SnowMmPerHour: f64(1.27),
		WeatherMain: "Snow", WeatherDescription: "light snow",
	}
	bare := WeatherSample{LocationID: "bearvalley", At: t0.Add(15 * time.Minute), TemperatureCelsius: 1.5, FeelsLikeCelsius: 1.5, HumidityPercent: 80}
	other := WeatherSample{LocationID: "murphys", At: t0, TemperatureCelsius: 9.1, FeelsLikeCelsius: 8, HumidityPercent: 60, WeatherMain: "Rain"}
	// The re-offer of a stored reading is ignored, whatever it now carries;
	// a row without a location or a time is dropped.
	reoffer := full
	reoffer.TemperatureCelsius = 99
	require.NoError(t, s.InsertWeatherSamples(ctx, []WeatherSample{full, bare, other, reoffer, {At: t0}, {LocationID: "x"}}))
	require.NoError(t, s.InsertWeatherSamples(ctx, []WeatherSample{reoffer}))
	require.NoError(t, s.InsertWeatherSamples(ctx, nil))

	got, err := s.WeatherSamples(ctx, "bearvalley", t0, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, full, got[0])
	assert.Equal(t, bare, got[1])
	assert.Nil(t, got[1].RainMmPerHour)
	assert.Nil(t, got[1].WindDirectionDegrees)

	// [from, to): the end is exclusive, and another location's rows are not read.
	got, err = s.WeatherSamples(ctx, "bearvalley", t0, t0.Add(15*time.Minute))
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestWeatherSampleCoverageAndPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	var batch []WeatherSample
	for i := range 5 {
		batch = append(batch, WeatherSample{LocationID: "arnold", At: t0.Add(time.Duration(i) * 15 * time.Minute), TemperatureCelsius: float64(i)})
	}
	batch = append(batch, WeatherSample{LocationID: "murphys", At: t0})
	require.NoError(t, s.InsertWeatherSamples(ctx, batch))

	first, last, n, err := s.WeatherSampleCoverage(ctx, "arnold", t0.Add(15*time.Minute), t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, t0.Add(15*time.Minute), first)
	assert.Equal(t, t0.Add(45*time.Minute), last)

	first, last, n, err = s.WeatherSampleCoverage(ctx, "arnold", t0.Add(-time.Hour), t0)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, first.IsZero() && last.IsZero())

	pruned, err := s.PruneWeatherSamples(ctx, t0.Add(30*time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 3, pruned, "two of arnold's and murphys's one are older than the cutoff")
	_, _, n, err = s.WeatherSampleCoverage(ctx, "arnold", t0, t0.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}
