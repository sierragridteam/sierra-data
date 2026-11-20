package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/weather"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// fakeWxReadings is the weather service's last readings, by location id.
type fakeWxReadings map[string]services.LocationReading

func (f fakeWxReadings) CurrentReadings() map[string]services.LocationReading { return f }

func weatherLocationConfig() *config.Config {
	cfg := testConfig()
	cfg.Weather.RefreshInterval = 15 * time.Minute
	cfg.Weather.Locations = []config.WeatherLocation{
		{ID: "murphys", Name: "Murphys, CA", Coordinates: config.Coordinates{Latitude: 38.139117, Longitude: -120.456111}},
		{ID: "bearvalley", Name: "Bear Valley, CA", Coordinates: config.Coordinates{Latitude: 38.461045, Longitude: -120.042368}},
	}
	return cfg
}

func fptr(v float64) *float64 { return &v }
func iptr(v int32) *int32     { return &v }

// A sample is the reading as reported, stamped with the reading's own time
// to the second; a location not yet read has none; a location that is not
// configured is not sampled.
func TestWeatherSamplesFromReadings(t *testing.T) {
	cfg := weatherLocationConfig()
	at := time.Date(2026, 11, 20, 8, 0, 3, 987654321, time.FixedZone("PST", -8*3600))
	got := WeatherSamples(cfg.Weather.Locations, fakeWxReadings{
		"bearvalley": {At: at, Reading: weather.Reading{
			TemperatureCelsius: -0.62, FeelsLikeCelsius: -5.4, HumidityPercent: 93,
			WindSpeedKmh: 16.6, WindDirectionDegrees: iptr(250), WindGustKmh: fptr(41.8),
			VisibilityMeters: iptr(800), SnowMmPerHour: fptr(1.27),
			WeatherMain: "Snow", WeatherDescription: "light snow",
		}},
		"retired": {At: at},
	})
	require.Len(t, got, 1, "murphys has no reading; retired is not configured")
	assert.Equal(t, store.WeatherSample{
		LocationID: "bearvalley", At: time.Date(2026, 11, 20, 16, 0, 3, 0, time.UTC),
		TemperatureCelsius: -0.62, FeelsLikeCelsius: -5.4, HumidityPercent: 93,
		WindSpeedKmh: 16.6, WindDirectionDegrees: iptr(250), WindGustKmh: fptr(41.8),
		VisibilityMeters: iptr(800), SnowMmPerHour: fptr(1.27),
		WeatherMain: "Snow", WeatherDescription: "light snow",
	}, got[0])
}

// One event per configured location, from configuration alone: no reading
// and no derived state is on it.
func TestWeatherLocationEvents(t *testing.T) {
	cfg := weatherLocationConfig()
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	n := NewWeatherLocationNormalizer(cfg, fakeWxReadings{
		"murphys":    {At: now.Add(-5 * time.Minute), Reading: weather.Reading{TemperatureCelsius: 9}},
		"bearvalley": {At: now.Add(-5 * time.Minute), Reading: weather.Reading{TemperatureCelsius: -3, WeatherMain: "Snow"}},
	})
	n.now = func() time.Time { return now }
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)
	require.Len(t, res.Events, 2)

	ev := res.Events[1]
	assert.Equal(t, "weather:bearvalley", ev.GetId())
	assert.Equal(t, gridv1.Layer_WEATHER, ev.GetLayer())
	assert.Equal(t, gridv1.Severity_INFO, ev.GetSeverity())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, "Weather at Bear Valley, CA", ev.GetHeadline())
	assert.Equal(t, "weather", ev.GetCategory())
	assert.Equal(t, []string{"town:bearvalley"}, ev.GetPlaceIds())
	assert.Equal(t, WeatherSourceID, ev.GetProvenance().GetSourceId())
	assert.Equal(t, 38.461045, ev.GetGeometry().GetCentroid().GetLat())
	assert.Equal(t, &gridv1.WeatherDetail{LocationId: "bearvalley", LocationName: "Bear Valley, CA"}, ev.GetWeather())

	// The reading is not on the event: a different one is the same content.
	before := store.ContentHash(ev)
	n.readings = fakeWxReadings{
		"murphys":    {At: now, Reading: weather.Reading{TemperatureCelsius: 12}},
		"bearvalley": {At: now, Reading: weather.Reading{TemperatureCelsius: 4, WeatherMain: "Rain"}},
	}
	n.now = func() time.Time { return now.Add(15 * time.Minute) }
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, before, store.ContentHash(res.Events[1]))
}

// The source's health is the readings' age. The events stand either way.
func TestWeatherLocationHealth(t *testing.T) {
	cfg := weatherLocationConfig()
	start := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	readings := fakeWxReadings{}
	n := NewWeatherLocationNormalizer(cfg, readings)
	n.started = start
	at := func(d time.Duration) { n.now = func() time.Time { return start.Add(d) } }
	poll := func() *PollResult {
		t.Helper()
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		require.Len(t, res.Events, 2, "the events stand whatever the readings")
		return res
	}

	// A new process has read nothing yet: not a failure until it has had
	// time to.
	at(time.Minute)
	assert.Empty(t, poll().PerSource)
	at(23 * time.Minute)
	err := poll().PerSource[WeatherSourceID]
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no reading of murphys yet")
	assert.Contains(t, err.Error(), "no reading of bearvalley yet")

	// Both read: healthy. One whose fetch keeps failing: named, with its age.
	readings["murphys"] = services.LocationReading{At: start.Add(20 * time.Minute)}
	readings["bearvalley"] = services.LocationReading{At: start.Add(20 * time.Minute)}
	assert.Empty(t, poll().PerSource)
	readings["murphys"] = services.LocationReading{At: start.Add(50 * time.Minute)}
	at(51 * time.Minute)
	err = poll().PerSource[WeatherSourceID]
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bearvalley last read 31m0s ago")
	assert.NotContains(t, err.Error(), "murphys")

	// No locations is a configuration error, never an empty success (which
	// would let the sweep end every location's event).
	cfg.Weather.Locations = nil
	_, pollErr := n.Poll(testCtx(), &fakePrior{})
	assert.Error(t, pollErr)
}

// Through the scheduler and the store: the event is written once and never
// revised while readings move; a failing source ends nothing; a location
// removed from configuration is resolved.
func TestWeatherLocationLifecycle(t *testing.T) {
	ctx := testCtx()
	cfg := weatherLocationConfig()
	st := newSchedStore(t)
	seedSchedSources(t, st, WeatherSourceID)
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{WeatherSourceID: {Disappearance: store.DisappearanceResolve}},
	})
	readings := fakeWxReadings{
		"murphys":    {At: time.Now(), Reading: weather.Reading{TemperatureCelsius: 9}},
		"bearvalley": {At: time.Now(), Reading: weather.Reading{TemperatureCelsius: -3}},
	}
	n := NewWeatherLocationNormalizer(cfg, readings)
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: n, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	readings["bearvalley"] = services.LocationReading{At: time.Now(), Reading: weather.Reading{TemperatureCelsius: 2, WeatherMain: "Rain"}}
	sched.tick(ctx, spec, ps)
	assert.Equal(t, uint32(1), eventRevision(t, st, "weather:bearvalley"), "a new reading is a sample, not a revision")
	got, err := st.GetEvent(ctx, "weather:bearvalley")
	require.NoError(t, err)
	assert.Contains(t, got.GetPlaceIds(), "town:bearvalley")

	// Readings go stale: the source fails, the events stay ACTIVE.
	readings["murphys"] = services.LocationReading{At: time.Now().Add(-2 * time.Hour)}
	readings["bearvalley"] = services.LocationReading{At: time.Now().Add(-2 * time.Hour)}
	sched.tick(ctx, spec, ps)
	got, err = st.GetEvent(ctx, "weather:bearvalley")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, got.GetStatus())
	assert.Equal(t, uint32(1), got.GetRevision())

	// A location leaves the configuration: with a healthy poll its event ends.
	readings["murphys"] = services.LocationReading{At: time.Now()}
	cfg.Weather.Locations = cfg.Weather.Locations[:1]
	sched.tick(ctx, spec, ps)
	got, err = st.GetEvent(ctx, "weather:bearvalley")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, got.GetStatus())
	kept, err := st.GetEvent(ctx, "weather:murphys")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, kept.GetStatus())
}

// One sampler tick writes each location's reading once: the same reading
// offered again is no new row, a new reading is one, and old rows are pruned.
func TestWeatherSamplerTick(t *testing.T) {
	ctx := testCtx()
	cfg := weatherLocationConfig()
	st := newSchedStore(t)
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	readings := fakeWxReadings{
		"murphys":    {At: t0, Reading: weather.Reading{TemperatureCelsius: 9.1, WeatherMain: "Clouds"}},
		"bearvalley": {At: t0.Add(2 * time.Second), Reading: weather.Reading{TemperatureCelsius: -0.62, SnowMmPerHour: fptr(1.27), WeatherMain: "Snow"}},
	}
	now := t0.Add(time.Minute)
	sched := NewScheduler(st, SchedulerConfig{WeatherSampling: WeatherSampling{
		Interval: WeatherSampleInterval, Retention: 400 * 24 * time.Hour,
		Sample: func(context.Context) ([]store.WeatherSample, error) {
			return WeatherSamples(cfg.Weather.Locations, readings), nil
		},
	}})
	sched.now = func() time.Time { return now }
	count := func(loc string) int {
		t.Helper()
		_, _, n, err := st.WeatherSampleCoverage(ctx, loc, t0.Add(-1000*24*time.Hour), t0.Add(24*time.Hour))
		require.NoError(t, err)
		return n
	}

	// An old row, to be pruned by the first tick.
	require.NoError(t, st.InsertWeatherSamples(ctx, []store.WeatherSample{{LocationID: "murphys", At: t0.Add(-401 * 24 * time.Hour)}}))
	lastPrune := sched.sampleWeather(ctx, time.Time{})
	assert.Equal(t, now, lastPrune)
	assert.Equal(t, 1, count("murphys"), "the reading, and the 401-day-old row gone")
	assert.Equal(t, 1, count("bearvalley"))

	// Five minutes on: murphys's fetch failed (same reading), bear valley was read again.
	now = t0.Add(6 * time.Minute)
	readings["bearvalley"] = services.LocationReading{At: t0.Add(5 * time.Minute), Reading: weather.Reading{TemperatureCelsius: -0.9, WeatherMain: "Snow"}}
	assert.Equal(t, lastPrune, sched.sampleWeather(ctx, lastPrune), "pruned within the day: not again")
	assert.Equal(t, 1, count("murphys"), "the same reading is not a second sample")
	assert.Equal(t, 2, count("bearvalley"))

	got, err := st.WeatherSamples(ctx, "bearvalley", t0, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, -0.62, got[0].TemperatureCelsius)
	assert.Equal(t, 1.27, *got[0].SnowMmPerHour)
	assert.Equal(t, -0.9, got[1].TemperatureCelsius)
	assert.Nil(t, got[1].SnowMmPerHour)
}
