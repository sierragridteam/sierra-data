-- Grid store schema version 8: weather_samples.
--
-- One row per weather location per successful read of current conditions
-- (OpenWeatherMap, about every 15 minutes): the series behind the location's
-- WEATHER event, served by GET /api/v1/events/{id}/samples. The pattern of
-- road_samples: the event holds state, the table holds every reading.
--
-- Values are as the source reported them, not the whole numbers
-- /api/v1/conditions serves: a freezing threshold cannot be checked against
-- a temperature cut to a whole degree. NULL is "the response did not carry
-- it" (OpenWeatherMap leaves the rain and snow blocks out when it reports
-- none, and reports a gust only where it has one), never a measured zero.
--
-- A new table: the previous release neither reads nor writes it.
CREATE TABLE weather_samples (
  location_id         text NOT NULL,           -- weather.locations[].id
  at                  bigint NOT NULL,         -- when the reading was taken (unix seconds)
  temp_c              double precision NOT NULL,
  feels_like_c        double precision NOT NULL,
  humidity_pct        integer NOT NULL,
  wind_kmh            double precision NOT NULL,
  wind_deg            integer,                 -- direction the wind blows from
  gust_kmh            double precision,
  visibility_m        integer,
  rain_mm_h           double precision,        -- over the last hour
  snow_mm_h           double precision,
  weather_main        text NOT NULL DEFAULT '',        -- condition group, "Snow"
  weather_description text NOT NULL DEFAULT '',        -- "light snow"
  PRIMARY KEY (location_id, at)
);
