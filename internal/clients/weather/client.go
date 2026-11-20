package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/sierragridteam/sierra-data/api/v1"
)

// HTTPDoer interface for HTTP clients (for testability)
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client provides access to OpenWeatherMap API
// Implementation per research.md lines 68-82
type Client struct {
	apiKey     string
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a new OpenWeatherMap API client
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: "https://api.openweathermap.org",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientWithHTTPDoer creates a new client with a custom HTTP client (for testing)
func NewClientWithHTTPDoer(apiKey, baseURL string, httpClient HTTPDoer) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// GetCurrentWeather retrieves current weather conditions for coordinates
// Endpoint per research.md line 92
func (c *Client) GetCurrentWeather(ctx context.Context, coordinates *api.Coordinates) (*api.WeatherData, error) {
	data, _, err := c.GetCurrentReading(ctx, coordinates)
	return data, err
}

// Reading is one location's current weather as OpenWeatherMap reported it,
// before the whole-number conversion WeatherData applies (which also drops
// the gust and the precipitation rates). It is what the weather sample
// archive stores (store.WeatherSample): a freezing threshold cannot be
// checked against a temperature cut to a whole degree. A pointer is nil when
// the response did not carry the field.
type Reading struct {
	TemperatureCelsius float64 // to 0.01, as reported
	FeelsLikeCelsius   float64
	HumidityPercent    int32
	WindSpeedKmh       float64 // from m/s, to 0.1
	// Direction the wind blows FROM; nil when the response had no wind block
	// direction (OpenWeatherMap reports 0 for calm and for north alike).
	WindDirectionDegrees *int32
	WindGustKmh          *float64 // reported "where available"
	VisibilityMeters     *int32   // OpenWeatherMap caps it at 10,000
	// Precipitation over the last hour, mm. OpenWeatherMap leaves the block
	// out when it reports none, so nil is "nothing reported", not a measured 0.
	RainMmPerHour *float64
	SnowMmPerHour *float64
	// Condition group and description, e.g. "Snow" and "light snow".
	WeatherMain        string
	WeatherDescription string
}

// GetCurrentReading is GetCurrentWeather with the reading beside it: one
// fetch, the same response read both ways.
func (c *Client) GetCurrentReading(ctx context.Context, coordinates *api.Coordinates) (*api.WeatherData, *Reading, error) {
	response, err := c.fetchCurrent(ctx, coordinates)
	if err != nil {
		return nil, nil, err
	}
	data, err := c.processCurrentWeatherResponse(*response)
	if err != nil {
		return nil, nil, err
	}
	return data, readingFrom(*response), nil
}

// round rounds v to the given number of decimals. The response's numbers are
// decoded as float32, so 3.4 would otherwise widen to 3.4000000953674316.
func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

func readingFrom(response OpenWeatherCurrentResponse) *Reading {
	r := &Reading{
		TemperatureCelsius: round(float64(response.Main.Temp), 2),
		FeelsLikeCelsius:   round(float64(response.Main.FeelsLike), 2),
		HumidityPercent:    response.Main.Humidity,
		WindSpeedKmh:       round(float64(response.Wind.Speed)*3.6, 1),
		VisibilityMeters:   response.Visibility,
	}
	if len(response.Weather) > 0 {
		r.WeatherMain = response.Weather[0].Main
		r.WeatherDescription = response.Weather[0].Description
	}
	if response.Wind.Deg != nil {
		d := *response.Wind.Deg
		r.WindDirectionDegrees = &d
	}
	if response.Wind.Gust != nil {
		g := round(float64(*response.Wind.Gust)*3.6, 1)
		r.WindGustKmh = &g
	}
	if response.Rain != nil && response.Rain.OneHour != nil {
		v := round(float64(*response.Rain.OneHour), 2)
		r.RainMmPerHour = &v
	}
	if response.Snow != nil && response.Snow.OneHour != nil {
		v := round(float64(*response.Snow.OneHour), 2)
		r.SnowMmPerHour = &v
	}
	return r
}

// fetchCurrent reads /data/2.5/weather for coordinates.
func (c *Client) fetchCurrent(ctx context.Context, coordinates *api.Coordinates) (*OpenWeatherCurrentResponse, error) {
	// Build URL with query parameters
	params := url.Values{}
	params.Set("lat", fmt.Sprintf("%.6f", coordinates.Latitude))
	params.Set("lon", fmt.Sprintf("%.6f", coordinates.Longitude))
	params.Set("appid", c.apiKey)
	params.Set("units", "metric") // Get temperature in Celsius

	requestURL := fmt.Sprintf("%s/data/2.5/weather?%s", c.baseURL, params.Encode())

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Execute request with rate limiting awareness (60/minute from research.md line 99)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle rate limiting and errors per research.md line 100
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limit exceeded (60/minute)")
	}
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("invalid API key")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var response OpenWeatherCurrentResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	// A 200 with no `main` block would read as 0 C, calm and dry: a reading
	// nobody took. OpenWeatherMap stamps every real answer with `dt`.
	if response.Main == nil || response.Dt == 0 {
		return nil, fmt.Errorf("response carries no current weather (no main block or dt)")
	}
	return &response, nil
}

// GetWeatherAlerts retrieves weather alerts using One Call API 3.0.
//
// NOT used by the server: One Call 3.0 has a 1,000 calls/day free cap, and for
// US locations its alerts are relabeled NWS data — the server sources alerts
// from api.weather.gov directly (see internal/services/weather_nws.go). This
// method is kept only for the cmd/test-weather diagnostic CLI; do not wire it
// back into a refresh path.
func (c *Client) GetWeatherAlerts(ctx context.Context, coordinates *api.Coordinates) ([]*api.WeatherAlert, error) {
	// Build URL for One Call API with alerts
	params := url.Values{}
	params.Set("lat", fmt.Sprintf("%.6f", coordinates.Latitude))
	params.Set("lon", fmt.Sprintf("%.6f", coordinates.Longitude))
	params.Set("appid", c.apiKey)
	params.Set("exclude", "minutely,hourly,daily") // Only get current + alerts

	requestURL := fmt.Sprintf("%s/data/3.0/onecall?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create alerts request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute alerts request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limit exceeded (60/minute)")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("alerts API error %d: %s", resp.StatusCode, string(body))
	}

	var response OpenWeatherOneCallResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode alerts response: %w", err)
	}

	return c.processWeatherAlerts(response.Alerts)
}

// processCurrentWeatherResponse converts OpenWeatherMap response to our WeatherData format
// Mapping per data-model.md lines 123-146
func (c *Client) processCurrentWeatherResponse(response OpenWeatherCurrentResponse) (*api.WeatherData, error) {
	// Extract primary weather condition
	var weatherMain, weatherDescription, weatherIcon string
	if len(response.Weather) > 0 {
		weatherMain = response.Weather[0].Main
		weatherDescription = response.Weather[0].Description
		weatherIcon = response.Weather[0].Icon
	}

	// Absent fields read as 0, as they always have on this message.
	var windDeg, visibilityKm int32
	if response.Wind.Deg != nil {
		windDeg = *response.Wind.Deg
	}
	if response.Visibility != nil {
		visibilityKm = *response.Visibility / 1000 // meters to km
	}
	if response.Main == nil {
		response.Main = &OpenWeatherMain{}
	}

	return &api.WeatherData{
		LocationId:           "", // Will be set by calling service
		LocationName:         response.Name,
		WeatherMain:          weatherMain,
		WeatherDescription:   weatherDescription,
		WeatherIcon:          weatherIcon,
		TemperatureCelsius:   int32(response.Main.Temp),      // Round to int
		FeelsLikeCelsius:     int32(response.Main.FeelsLike), // Round to int
		HumidityPercent:      response.Main.Humidity,
		WindSpeedKmh:         int32(response.Wind.Speed * 3.6), // Convert m/s to km/h
		WindDirectionDegrees: windDeg,
		VisibilityKm:         visibilityKm,
		Alerts:               nil, // Alerts fetched separately
	}, nil
}

// processWeatherAlerts converts OpenWeatherMap alerts to our WeatherAlert format
// Mapping per data-model.md lines 169-181
func (c *Client) processWeatherAlerts(alerts []OpenWeatherAlert) ([]*api.WeatherAlert, error) {
	var processedAlerts []*api.WeatherAlert

	for _, alert := range alerts {
		// Generate unique ID from sender + event + start time
		id := fmt.Sprintf("%s_%s_%d", alert.SenderName, alert.Event, alert.Start)

		processedAlert := &api.WeatherAlert{
			Id:          id,
			SenderName:  alert.SenderName,
			Event:       alert.Event,
			Description: alert.Description,
			Tags:        alert.Tags,
		}
		if alert.Start > 0 {
			processedAlert.StartTime = timestamppb.New(time.Unix(alert.Start, 0).UTC())
		}
		if alert.End > 0 {
			processedAlert.EndTime = timestamppb.New(time.Unix(alert.End, 0).UTC())
		}

		processedAlerts = append(processedAlerts, processedAlert)
	}

	return processedAlerts, nil
}

// OpenWeatherCurrentResponse represents the current weather API response
type OpenWeatherCurrentResponse struct {
	Coord      OpenWeatherCoord     `json:"coord"`
	Weather    []OpenWeatherWeather `json:"weather"`
	Main       *OpenWeatherMain     `json:"main"`
	Wind       OpenWeatherWind      `json:"wind"`
	Clouds     OpenWeatherClouds    `json:"clouds"`
	Visibility *int32               `json:"visibility"`
	Rain       *OpenWeatherPrecip   `json:"rain"`
	Snow       *OpenWeatherPrecip   `json:"snow"`
	Name       string               `json:"name"`
	Dt         int64                `json:"dt"`
}

// OpenWeatherOneCallResponse represents One Call API response with alerts
type OpenWeatherOneCallResponse struct {
	Lat    float64            `json:"lat"`
	Lon    float64            `json:"lon"`
	Alerts []OpenWeatherAlert `json:"alerts,omitempty"`
}

// OpenWeatherCoord represents coordinates in response
type OpenWeatherCoord struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// OpenWeatherWeather represents weather condition
type OpenWeatherWeather struct {
	Main        string `json:"main"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

// OpenWeatherMain represents main weather data
type OpenWeatherMain struct {
	Temp      float32 `json:"temp"`
	FeelsLike float32 `json:"feels_like"`
	Pressure  int32   `json:"pressure"`
	Humidity  int32   `json:"humidity"`
}

// OpenWeatherWind represents wind data
type OpenWeatherWind struct {
	Speed float32  `json:"speed"`
	Deg   *int32   `json:"deg"`
	Gust  *float32 `json:"gust"` // m/s, where available
}

// OpenWeatherPrecip is the `rain` or `snow` block: precipitation over the
// last hour, mm. Present only when OpenWeatherMap reports some.
type OpenWeatherPrecip struct {
	OneHour *float32 `json:"1h"`
}

// OpenWeatherClouds represents cloud cover
type OpenWeatherClouds struct {
	All int32 `json:"all"`
}

// OpenWeatherAlert represents weather alert from One Call API
// Structure per data-model.md lines 169-181
type OpenWeatherAlert struct {
	SenderName  string   `json:"sender_name"`
	Event       string   `json:"event"`
	Start       int64    `json:"start"`
	End         int64    `json:"end"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}
