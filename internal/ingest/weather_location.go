package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// WeatherSourceID is the registry row of the weather-location events:
// OpenWeatherMap's current conditions, as the weather service reads them.
const WeatherSourceID = "openweather"

// Provenance of a weather-location event. cmd/server's gridSourceInfo row
// for WeatherSourceID carries the same attribution.
const (
	WeatherSourceName  = "OpenWeatherMap"
	WeatherAttribution = "OpenWeatherMap"
	WeatherSourceURL   = "https://openweathermap.org/"
)

// WeatherSampleInterval is how often the weather sampler looks for new
// readings. It is shorter than the weather refresh (15 min) on purpose: a
// sample is stamped with its reading's time, so looking often costs one
// in-memory read and an insert the store ignores, and no refresh is missed
// when the two clocks slip. How far apart samples normally are is the
// refresh interval, not this.
const WeatherSampleInterval = 5 * time.Minute

// WeatherReadings is the weather service's last successful read of each
// location (services.WeatherService.CurrentReadings). It never fetches.
type WeatherReadings interface {
	CurrentReadings() map[string]services.LocationReading
}

// WeatherEventID is the id of a weather location's event.
func WeatherEventID(locationID string) string { return "weather:" + locationID }

// WeatherSamples turns the service's current readings into sample rows for
// the configured locations, in configuration order. A location with no
// reading since the process started has no row. A sample's time is its
// reading's, to the second: offering the same reading again is the same row,
// which the store ignores, so a location whose fetch keeps failing leaves a
// gap in its series instead of a repeated value.
func WeatherSamples(locations []config.WeatherLocation, readings map[string]services.LocationReading) []store.WeatherSample {
	var out []store.WeatherSample
	for _, loc := range locations {
		lr, ok := readings[loc.ID]
		if !ok || lr.At.IsZero() {
			continue
		}
		r := lr.Reading
		out = append(out, store.WeatherSample{
			LocationID:           loc.ID,
			At:                   lr.At.Truncate(time.Second).UTC(),
			TemperatureCelsius:   r.TemperatureCelsius,
			FeelsLikeCelsius:     r.FeelsLikeCelsius,
			HumidityPercent:      r.HumidityPercent,
			WindSpeedKmh:         r.WindSpeedKmh,
			WindDirectionDegrees: r.WindDirectionDegrees,
			WindGustKmh:          r.WindGustKmh,
			VisibilityMeters:     r.VisibilityMeters,
			RainMmPerHour:        r.RainMmPerHour,
			SnowMmPerHour:        r.SnowMmPerHour,
			WeatherMain:          r.WeatherMain,
			WeatherDescription:   r.WeatherDescription,
		})
	}
	return out
}

// WeatherLocationNormalizer keeps one standing event per weather location
// (layer WEATHER, id "weather:<location id>"). It is the mesh-node and
// road-segment pattern with the state still to come: the event is the
// subject, and everything measured is a row of weather_samples, written by
// the scheduler's weather sampler and served by
// GET /api/v1/events/{id}/samples.
//
// The event carries NO reading and NO derived state (decided 2026-11-19,
// workspace note 2026-11-19-weather-states-and-samples.md): it exists so the
// samples have a way in before the states are chosen. Everything on it comes
// from configuration (id, name, coordinates), so its hash never moves and it
// is written once per location; a renamed or moved location is one revision.
// The freezing and precipitation states will be fields of WeatherDetail and
// revisions of this event, behind their own flag. Until then do not put a
// reading on it: one number here is a revision every refresh, which is what
// the sample table is for.
//
// The geometry is the location's point (the readings are for that
// coordinate), and the town place is preset: a town is a point place, which
// no geometry match attaches.
//
// Health is the readings' age, since this poller makes no fetch of its own:
// the source fails while a configured location has no reading younger than
// 1.5 times the weather refresh interval, the rule /api/v1/conditions uses
// for weatherStatus. A location not yet read is not a failure until the
// process has been up that long (a new writer's first poll runs before the
// weather cache is warm). The events stand either way: a failed poll never
// ends one, and a location removed from configuration is resolved by the
// sweep.
type WeatherLocationNormalizer struct {
	cfg      *config.Config
	readings WeatherReadings
	now      func() time.Time
	started  time.Time
}

// NewWeatherLocationNormalizer wires the poller.
func NewWeatherLocationNormalizer(cfg *config.Config, readings WeatherReadings) *WeatherLocationNormalizer {
	return &WeatherLocationNormalizer{cfg: cfg, readings: readings, now: time.Now, started: time.Now()}
}

// SourceIDs implements Normalizer.
func (n *WeatherLocationNormalizer) SourceIDs() []string { return []string{WeatherSourceID} }

// Poll implements Normalizer.
func (n *WeatherLocationNormalizer) Poll(_ context.Context, _ Prior) (*PollResult, error) {
	locations := n.cfg.Weather.Locations
	if len(locations) == 0 {
		return nil, errEmptyScope("weather locations")
	}
	now := n.now()
	staleAfter := n.cfg.Weather.RefreshInterval * 3 / 2
	readings := n.readings.CurrentReadings()
	res := &PollResult{}
	var problems []string
	for _, loc := range locations {
		res.Events = append(res.Events, weatherLocationEvent(loc, now))
		if staleAfter <= 0 {
			continue
		}
		lr, ok := readings[loc.ID]
		switch {
		case !ok:
			if now.Sub(n.started) > staleAfter {
				problems = append(problems, "no reading of "+loc.ID+" yet")
			}
		case now.Sub(lr.At) > staleAfter:
			problems = append(problems, fmt.Sprintf("%s last read %s ago", loc.ID, now.Sub(lr.At).Round(time.Minute)))
		}
	}
	if len(problems) > 0 {
		res.PerSource = map[string]error{WeatherSourceID: fmt.Errorf("weather locations: %s", strings.Join(problems, "; "))}
	}
	return res, nil
}

func weatherLocationEvent(loc config.WeatherLocation, now time.Time) *gridv1.Event {
	ev := NewEvent(WeatherEventID(loc.ID), gridv1.Layer_WEATHER, gridv1.Severity_INFO, gridv1.EventStatus_ACTIVE,
		"Weather at "+loc.Name)
	ev.Category = "weather"
	ev.AreaLabel = loc.Name
	ev.Geometry = GeometryFromPoint(loc.Coordinates.Latitude, loc.Coordinates.Longitude)
	// Outside the hash: it stays the time the event was first written.
	ev.ObservedAt = tsProto(now)
	ev.Provenance = NewProvenance(WeatherSourceID, WeatherSourceName, WeatherAttribution, WeatherSourceURL)
	ev.Provenance.FetchedAt = tsProto(now)
	ev.Detail = &gridv1.Event_Weather{Weather: &gridv1.WeatherDetail{LocationId: loc.ID, LocationName: loc.Name}}
	ev.PlaceIds = []string{"town:" + loc.ID}
	return ev
}
