package services

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/clients/weather"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// openWeatherDoer answers /data/2.5/weather by latitude: a body, or a failure
// for a latitude listed in down.
type openWeatherDoer struct {
	temp map[string]string // "lat" query value -> temp
	down map[string]bool
}

func (d *openWeatherDoer) Do(req *http.Request) (*http.Response, error) {
	lat := req.URL.Query().Get("lat")
	if d.down[lat] {
		return nil, errors.New("simulated OpenWeatherMap outage")
	}
	body := `{"weather":[{"main":"Snow","description":"light snow","icon":"13n"}],
		"main":{"temp":` + d.temp[lat] + `,"feels_like":-4,"humidity":90},
		"wind":{"speed":2.5,"deg":200},"visibility":5000,"snow":{"1h":0.5},"dt":1795161600}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// CurrentReadings is each town's last successful read, unrounded, with its
// own time: a town whose fetch fails keeps the reading and the time it had,
// so the sampler sees the same reading and writes no new sample for it.
func TestCurrentReadingsKeepTheLastGoodRead(t *testing.T) {
	doer := &openWeatherDoer{temp: map[string]string{"38.100000": "-0.62", "38.200000": "1.49"}, down: map[string]bool{}}
	c := cache.NewCache()
	require.NoError(t, c.Set("nws:alerts", []nws.Alert{}, time.Hour, "nws_alerts"))
	cfg := &config.Config{
		OpenWeather: config.OpenWeatherClient{APIKey: "test"},
		Weather: config.WeatherConfig{
			RefreshInterval: time.Minute,
			NWS:             config.NWSConfig{UserAgent: "test"},
			Locations: []config.WeatherLocation{
				{ID: "high", Name: "High", Coordinates: config.Coordinates{Latitude: 38.1, Longitude: -120}},
				{ID: "low", Name: "Low", Coordinates: config.Coordinates{Latitude: 38.2, Longitude: -120}},
			},
		},
	}
	svc := NewWeatherService(weather.NewClientWithHTTPDoer("test", "https://openweather.test", doer), nws.NewClient("test"), c, cfg)
	assert.Empty(t, svc.CurrentReadings(), "nothing read yet")

	data, err := svc.refreshWeatherData(testCtx())
	require.NoError(t, err)
	require.Len(t, data, 2)
	assert.Equal(t, int32(0), data[0].GetTemperatureCelsius(), "the served message is whole degrees")
	first := svc.CurrentReadings()
	require.Len(t, first, 2)
	assert.Equal(t, -0.62, first["high"].Reading.TemperatureCelsius, "the reading is not")
	assert.Equal(t, 1.49, first["low"].Reading.TemperatureCelsius)
	assert.Equal(t, 0.5, *first["high"].Reading.SnowMmPerHour)
	assert.Equal(t, svc.ReadingTimes()["high"], first["high"].At)

	// The next refresh: one town fails, the other is read again.
	doer.down["38.100000"] = true
	doer.temp["38.200000"] = "2.5"
	time.Sleep(5 * time.Millisecond)
	_, err = svc.refreshWeatherData(testCtx())
	require.NoError(t, err)
	second := svc.CurrentReadings()
	assert.Equal(t, first["high"], second["high"], "the failed town keeps its reading and its time")
	assert.Equal(t, 2.5, second["low"].Reading.TemperatureCelsius)
	assert.True(t, second["low"].At.After(first["low"].At))
}
