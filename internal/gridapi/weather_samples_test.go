package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

func pf(v float64) *float64 { return &v }

// weatherEventService is a service holding one WEATHER event, weather:bv,
// with the clock at now. The test config's weather refresh is unset, so the
// cadence is the 15-minute default.
func weatherEventService(t *testing.T, now time.Time) (*Service, *GridServer, string) {
	t.Helper()
	s := newTestService(t)
	s.Now = func() time.Time { return now }
	require.NoError(t, s.Store.SeedSources(context.Background(), []store.SourceSeed{
		{ID: "openweather", Name: "OpenWeatherMap", PollInterval: 5 * time.Minute},
	}))
	upsert(t, s.Store, &gridv1.Event{
		Id: "weather:bv", Layer: gridv1.Layer_WEATHER, Category: "weather",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE, Headline: "Weather at Bear Valley, CA",
		Provenance: &gridv1.Provenance{SourceId: "openweather", SourceName: "OpenWeatherMap", FetchedAt: timestamppb.New(base)},
		Detail:     &gridv1.Event_Weather{Weather: &gridv1.WeatherDetail{LocationId: "bv", LocationName: "Bear Valley, CA"}},
	})
	return s, NewGridServer(s), "weather:bv"
}

// wx is a reading i quarter hours after sampleT0.
func wx(i int, temp float64, main, description string) store.WeatherSample {
	return store.WeatherSample{
		LocationID: "bv", At: sampleT0.Add(time.Duration(i) * 15 * time.Minute),
		TemperatureCelsius: temp, FeelsLikeCelsius: temp - 3, HumidityPercent: 90,
		WeatherMain: main, WeatherDescription: description,
	}
}

// The wire shape: a reading as reported (decimals, a temperature below zero),
// null for what the source did not report, and a bucket in the same shape.
func TestGetEventSamplesWeatherJSON(t *testing.T) {
	s, g, id := weatherEventService(t, sampleT0.Add(24*time.Hour))
	snow := wx(0, -0.62, "Snow", "light snow")
	snow.WindSpeedKmh, snow.WindDirectionDegrees, snow.WindGustKmh = 16.6, p32(250), pf(41.8)
	snow.VisibilityMeters, snow.SnowMmPerHour = p32(800), pf(1.27)
	dry := wx(1, 1.5, "Clouds", "overcast clouds") // calm; no gust, visibility or precipitation in the response
	rain := wx(2, 2.25, "Rain", "light rain")
	rain.WindSpeedKmh, rain.WindDirectionDegrees = 10, p32(270)
	rain.VisibilityMeters, rain.RainMmPerHour = p32(10000), pf(0.25)
	require.NoError(t, s.Store.InsertWeatherSamples(rpcBG(), []store.WeatherSample{snow, dry, rain}))
	from, to := sampleT0, sampleT0.Add(45*time.Minute)

	raw, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to))
	require.NoError(t, err)
	body, err := gatewaySummaryJSON.Marshal(raw)
	require.NoError(t, err)
	t.Logf("raw: %s", body)
	assert.JSONEq(t, `{
		"eventId": "weather:bv", "layer": "WEATHER",
		"from": "2026-11-10T00:00:00Z", "to": "2026-11-10T00:45:00Z",
		"coverage": {"first": "2026-11-10T00:00:00Z", "last": "2026-11-10T00:30:00Z", "samples": 3},
		"bucketSeconds": 0,
		"gaps": [],
		"road": [],
		"weather": [
			{"at": "2026-11-10T00:00:00Z", "samples": 1,
			 "temperatureCelsius": -0.62, "temperatureCelsiusMin": -0.62, "temperatureCelsiusMax": -0.62,
			 "feelsLikeCelsius": -3.62, "feelsLikeCelsiusMin": -3.62, "humidityPercent": 90,
			 "windSpeedKmh": 16.6, "windSpeedKmhMax": 16.6, "windGustKmhMax": 41.8, "windDirectionDegrees": 250,
			 "visibilityKm": 0.8, "visibilityKmMin": 0.8,
			 "rainMmPerHour": null, "rainMmPerHourMax": null, "snowMmPerHour": 1.27, "snowMmPerHourMax": 1.27,
			 "conditions": [{"weatherMain": "Snow", "weatherDescription": "light snow", "samples": 1}]},
			{"at": "2026-11-10T00:15:00Z", "samples": 1,
			 "temperatureCelsius": 1.5, "temperatureCelsiusMin": 1.5, "temperatureCelsiusMax": 1.5,
			 "feelsLikeCelsius": -1.5, "feelsLikeCelsiusMin": -1.5, "humidityPercent": 90,
			 "windSpeedKmh": 0, "windSpeedKmhMax": 0, "windGustKmhMax": null, "windDirectionDegrees": null,
			 "visibilityKm": null, "visibilityKmMin": null,
			 "rainMmPerHour": null, "rainMmPerHourMax": null, "snowMmPerHour": null, "snowMmPerHourMax": null,
			 "conditions": [{"weatherMain": "Clouds", "weatherDescription": "overcast clouds", "samples": 1}]},
			{"at": "2026-11-10T00:30:00Z", "samples": 1,
			 "temperatureCelsius": 2.25, "temperatureCelsiusMin": 2.25, "temperatureCelsiusMax": 2.25,
			 "feelsLikeCelsius": -0.75, "feelsLikeCelsiusMin": -0.75, "humidityPercent": 90,
			 "windSpeedKmh": 10, "windSpeedKmhMax": 10, "windGustKmhMax": null, "windDirectionDegrees": 270,
			 "visibilityKm": 10, "visibilityKmMin": 10,
			 "rainMmPerHour": 0.25, "rainMmPerHourMax": 0.25, "snowMmPerHour": null, "snowMmPerHourMax": null,
			 "conditions": [{"weatherMain": "Rain", "weatherDescription": "light rain", "samples": 1}]}
		]}`, string(body))

	// One bucket of all three: means over the readings that carried the
	// figure, the extremes, and every condition seen.
	bucketed, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, 1))
	require.NoError(t, err)
	assert.Equal(t, int32(3600), bucketed.GetBucketSeconds())
	require.Len(t, bucketed.GetWeather(), 1)
	p := bucketed.GetWeather()[0]
	assert.Equal(t, sampleT0, p.GetAt().AsTime())
	assert.Equal(t, int32(3), p.GetSamples())
	assert.Equal(t, 1.04, p.GetTemperatureCelsius().GetValue(), "(-0.62 + 1.5 + 2.25) / 3")
	assert.Equal(t, -0.62, p.GetTemperatureCelsiusMin().GetValue())
	assert.Equal(t, 2.25, p.GetTemperatureCelsiusMax().GetValue())
	assert.Equal(t, -3.62, p.GetFeelsLikeCelsiusMin().GetValue())
	assert.Equal(t, 8.9, p.GetWindSpeedKmh().GetValue(), "calm counts: (16.6 + 0 + 10) / 3")
	assert.Equal(t, 16.6, p.GetWindSpeedKmhMax().GetValue())
	assert.Equal(t, 41.8, p.GetWindGustKmhMax().GetValue())
	assert.Equal(t, 5.4, p.GetVisibilityKm().GetValue(), "over the two readings that had one")
	assert.Equal(t, 0.8, p.GetVisibilityKmMin().GetValue())
	assert.Equal(t, 0.25, p.GetRainMmPerHour().GetValue(), "over the one reading that reported rain")
	assert.Equal(t, 1.27, p.GetSnowMmPerHourMax().GetValue())
	require.Len(t, p.GetConditions(), 3)
	assert.Equal(t, "Rain", p.GetConditions()[0].GetWeatherMain(), "equal counts: the latest first")
	assert.Equal(t, "Snow", p.GetConditions()[2].GetWeatherMain())
	assert.Empty(t, bucketed.GetRoad())
}

// A bucket's wind direction is the direction of the mean wind vector: 350
// and 10 degrees average to north, calm readings do not vote, and winds that
// cancel have no direction.
func TestWeatherBucketWindDirection(t *testing.T) {
	blow := func(i int, kmh float64, deg int32) store.WeatherSample {
		s := wx(i, 5, "Clear", "clear sky")
		s.WindSpeedKmh, s.WindDirectionDegrees = kmh, p32(deg)
		return s
	}
	one := func(samples ...store.WeatherSample) *gridv1.WeatherSamplePoint {
		pts := weatherSamplePoints(samples, 86400)
		require.Len(t, pts, 1)
		return pts[0]
	}
	assert.Equal(t, int32(0), one(blow(0, 10, 350), blow(1, 10, 10)).GetWindDirectionDegrees().GetValue())
	assert.NotNil(t, one(blow(0, 10, 350), blow(1, 10, 10)).GetWindDirectionDegrees(), "north is 0, not null")
	assert.Equal(t, int32(270), one(blow(0, 20, 270), blow(1, 0, 90)).GetWindDirectionDegrees().GetValue(), "calm has no direction")
	assert.Equal(t, int32(90), one(blow(0, 30, 90), blow(1, 10, 270)).GetWindDirectionDegrees().GetValue(), "weighted by speed")
	assert.Nil(t, one(blow(0, 10, 90), blow(1, 10, 270)).GetWindDirectionDegrees(), "opposite winds cancel")
	assert.Nil(t, one(wx(0, 5, "", "")).GetWindDirectionDegrees())
	assert.Empty(t, one(wx(0, 5, "", "")).GetConditions(), "a reading that named no condition")
}

// The most frequent condition is first, so a mostly-snowy bucket reads Snow.
func TestWeatherBucketConditionsOrder(t *testing.T) {
	pts := weatherSamplePoints([]store.WeatherSample{
		wx(0, 0, "Snow", "light snow"), wx(1, 0, "Rain", "light rain"), wx(2, 0, "Snow", "light snow"), wx(3, 0, "Snow", "snow"),
	}, 3600)
	require.Len(t, pts, 1)
	c := pts[0].GetConditions()
	require.Len(t, c, 3)
	assert.Equal(t, "light snow", c[0].GetWeatherDescription())
	assert.Equal(t, int32(2), c[0].GetSamples())
	assert.Equal(t, "snow", c[1].GetWeatherDescription(), "one each: the latest first")
	assert.Equal(t, "Rain", c[2].GetWeatherMain())
}

// Coverage and gaps are the shared envelope's, on the weather cadence: a
// location that was not read for an hour is a gap, one missed read is not.
// And the validator turns over when a reading lands.
func TestGetEventSamplesWeatherGapsAndETag(t *testing.T) {
	now := sampleT0.Add(3 * time.Hour)
	s, g, id := weatherEventService(t, now)
	// 00:00, 00:15, (00:30 missed), 00:45, then nothing until 02:45.
	require.NoError(t, s.Store.InsertWeatherSamples(rpcBG(), []store.WeatherSample{
		wx(0, 1, "Clear", "clear sky"), wx(1, 1, "Clear", "clear sky"), wx(3, 1, "Clear", "clear sky"), wx(11, 1, "Clear", "clear sky"),
	}))
	live := func(ctx context.Context) error {
		_, err := g.GetEventSamples(ctx, &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(sampleT0)})
		return err
	}
	got, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(sampleT0)})
	require.NoError(t, err)
	assert.Equal(t, int32(4), got.GetCoverage().GetSamples())
	require.Len(t, got.GetGaps(), 1)
	assert.Equal(t, sampleT0.Add(45*time.Minute), got.GetGaps()[0].GetFrom().AsTime())
	assert.Equal(t, sampleT0.Add(165*time.Minute), got.GetGaps()[0].GetTo().AsTime())

	tag := rpcTag(t, live)
	assert.True(t, rpcNotModified(t, tag, live))
	require.NoError(t, s.Store.InsertWeatherSamples(rpcBG(), []store.WeatherSample{wx(12, 1, "Clear", "clear sky")})) // 03:00 is now: outside [from, now)
	assert.True(t, rpcNotModified(t, tag, live))
	require.NoError(t, s.Store.InsertWeatherSamples(rpcBG(), []store.WeatherSample{{LocationID: "bv", At: now.Add(-time.Minute)}}))
	assert.False(t, rpcNotModified(t, tag, live), "a new reading in the window")

	// Another location's readings are not this event's.
	require.NoError(t, s.Store.InsertWeatherSamples(rpcBG(), []store.WeatherSample{{LocationID: "elsewhere", At: sampleT0.Add(time.Hour)}}))
	again, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(sampleT0)})
	require.NoError(t, err)
	assert.Equal(t, int32(5), again.GetCoverage().GetSamples())
}

// A WEATHER event with no samples yet answers an empty series and one gap,
// not a 404: the event is the way in from its first minute.
func TestGetEventSamplesWeatherEmpty(t *testing.T) {
	_, g, id := weatherEventService(t, sampleT0.Add(time.Hour))
	got, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(sampleT0)})
	require.NoError(t, err)
	assert.Equal(t, gridv1.Layer_WEATHER, got.GetLayer())
	assert.Zero(t, got.GetCoverage().GetSamples())
	assert.Nil(t, got.GetCoverage().GetFirst())
	assert.Empty(t, got.GetWeather())
	require.Len(t, got.GetGaps(), 1)
}
