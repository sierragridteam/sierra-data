package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// WeatherSample is one weather location at one successful read of current
// conditions, as the source reported it (weather.Reading): unrounded, and
// with nil where the response did not carry a field.
type WeatherSample struct {
	LocationID string
	At         time.Time

	TemperatureCelsius   float64
	FeelsLikeCelsius     float64
	HumidityPercent      int32
	WindSpeedKmh         float64
	WindDirectionDegrees *int32
	WindGustKmh          *float64
	VisibilityMeters     *int32
	// Precipitation over the last hour, mm; nil when none was reported.
	RainMmPerHour *float64
	SnowMmPerHour *float64

	WeatherMain        string
	WeatherDescription string
}

// InsertWeatherSamples appends samples. A (location, at) already stored is
// ignored: the sampler re-offers a location's last reading until the weather
// service reads it again, and a sample is immutable once written.
func (s *Store) InsertWeatherSamples(ctx context.Context, samples []WeatherSample) error {
	n := len(samples)
	loc, at := make([]string, 0, n), make([]int64, 0, n)
	temp, feels, wind := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
	humidity := make([]int32, 0, n)
	windDeg, vis := make([]*int32, 0, n), make([]*int32, 0, n)
	gust, rain, snow := make([]*float64, 0, n), make([]*float64, 0, n), make([]*float64, 0, n)
	main, desc := make([]string, 0, n), make([]string, 0, n)
	for _, r := range samples {
		if r.LocationID == "" || r.At.IsZero() {
			continue
		}
		loc, at = append(loc, r.LocationID), append(at, r.At.Unix())
		temp, feels, wind = append(temp, r.TemperatureCelsius), append(feels, r.FeelsLikeCelsius), append(wind, r.WindSpeedKmh)
		humidity = append(humidity, r.HumidityPercent)
		windDeg, vis = append(windDeg, r.WindDirectionDegrees), append(vis, r.VisibilityMeters)
		gust, rain, snow = append(gust, r.WindGustKmh), append(rain, r.RainMmPerHour), append(snow, r.SnowMmPerHour)
		main, desc = append(main, r.WeatherMain), append(desc, r.WeatherDescription)
	}
	if len(loc) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// One multi-row INSERT (unnest). A repeated (location, at) inside the
		// batch is ignored like one already stored: the first wins.
		if _, err := tx.ExecContext(ctx, `INSERT INTO weather_samples
			(location_id, at, temp_c, feels_like_c, humidity_pct, wind_kmh, wind_deg, gust_kmh,
			 visibility_m, rain_mm_h, snow_mm_h, weather_main, weather_description)
			SELECT location_id, at, temp_c, feels_like_c, humidity_pct, wind_kmh, wind_deg, gust_kmh,
			       visibility_m, rain_mm_h, snow_mm_h, weather_main, weather_description
			FROM unnest($1::text[], $2::bigint[], $3::float8[], $4::float8[], $5::int[], $6::float8[],
			            $7::int[], $8::float8[], $9::int[], $10::float8[], $11::float8[], $12::text[], $13::text[])
			     WITH ORDINALITY AS t(location_id, at, temp_c, feels_like_c, humidity_pct, wind_kmh, wind_deg,
			                          gust_kmh, visibility_m, rain_mm_h, snow_mm_h, weather_main, weather_description, ord)
			ORDER BY ord
			ON CONFLICT (location_id, at) DO NOTHING`,
			loc, at, temp, feels, humidity, wind, windDeg, gust, vis, rain, snow, main, desc); err != nil {
			return fmt.Errorf("store: insert %d weather samples: %w", len(loc), err)
		}
		return nil
	})
}

// WeatherSamples returns one location's samples in [from, to), oldest first.
func (s *Store) WeatherSamples(ctx context.Context, locationID string, from, to time.Time) ([]WeatherSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, temp_c, feels_like_c, humidity_pct, wind_kmh, wind_deg, gust_kmh,
			visibility_m, rain_mm_h, snow_mm_h, weather_main, weather_description
		FROM weather_samples
		WHERE location_id = $1 AND at >= $2 AND at < $3 ORDER BY at`, locationID, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: weather samples %s: %w", locationID, err)
	}
	defer rows.Close()
	var out []WeatherSample
	for rows.Next() {
		var at int64
		var windDeg, vis sql.NullInt32
		var gust, rain, snow sql.NullFloat64
		r := WeatherSample{LocationID: locationID}
		if err := rows.Scan(&at, &r.TemperatureCelsius, &r.FeelsLikeCelsius, &r.HumidityPercent, &r.WindSpeedKmh,
			&windDeg, &gust, &vis, &rain, &snow, &r.WeatherMain, &r.WeatherDescription); err != nil {
			return nil, fmt.Errorf("store: scan weather sample: %w", err)
		}
		r.At = time.Unix(at, 0).UTC()
		r.WindDirectionDegrees, r.VisibilityMeters = nullInt32(windDeg), nullInt32(vis)
		r.WindGustKmh, r.RainMmPerHour, r.SnowMmPerHour = nullFloat64(gust), nullFloat64(rain), nullFloat64(snow)
		out = append(out, r)
	}
	return out, rows.Err()
}

// WeatherSampleCoverage is what [from, to) holds for one location: how many
// samples, and the first and last (zero times when n is 0). One index-only
// read of the primary key, as RoadSampleCoverage: the samples endpoint's
// validator.
func (s *Store) WeatherSampleCoverage(ctx context.Context, locationID string, from, to time.Time) (first, last time.Time, n int, err error) {
	var lo, hi sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(at), MAX(at) FROM weather_samples
		WHERE location_id = $1 AND at >= $2 AND at < $3`, locationID, from.Unix(), to.Unix()).Scan(&n, &lo, &hi); err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("store: weather sample coverage %s: %w", locationID, err)
	}
	if n == 0 {
		return time.Time{}, time.Time{}, 0, nil
	}
	return time.Unix(lo.Int64, 0).UTC(), time.Unix(hi.Int64, 0).UTC(), n, nil
}

// PruneWeatherSamples deletes samples older than cutoff.
func (s *Store) PruneWeatherSamples(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM weather_samples WHERE at < $1`, cutoff.Unix())
		if err != nil {
			return fmt.Errorf("store: prune weather samples: %w", err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func nullInt32(v sql.NullInt32) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

func nullFloat64(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
