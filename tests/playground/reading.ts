// The whole measurement: read open-meteo, convert the WMO code, print the line. `Input` is
// the type genroc INFERRED for what process.yaml passes; `Output` is what its result_schema
// declares. Both are generated — run `genctl types -f script.yaml -f process.yaml`, or just
// apply.
//
// `fetch`, `console` and the node builtins typecheck because the authoring sandbox is a
// worker realm with node's globals; the DOM is not. See specs/script-tasks.md.
//
// Template literals need no escaping here: genctl doubles every `$` on splice, so `${…}` is
// JavaScript rather than a genroc interpolation. That is the whole point of the import.
import type { Input, Output } from "./reading.genroc";
import { appendFile } from "node:fs/promises";

// WMO weather codes, jen ty běžné — cokoliv jiného se vypíše číslem.
const CONDITIONS: Record<number, string> = {
  0: "jasno",
  1: "skoro jasno",
  2: "polojasno",
  3: "zataženo",
  45: "mlha",
  48: "namrzající mlha",
  51: "mrholení",
  53: "mrholení",
  55: "silné mrholení",
  61: "slabý déšť",
  63: "déšť",
  65: "silný déšť",
  71: "slabé sněžení",
  73: "sněžení",
  75: "husté sněžení",
  80: "přeháňky",
  81: "přeháňky",
  82: "silné přeháňky",
  95: "bouřka",
  96: "bouřka s kroupami",
  99: "bouřka s kroupami",
};

type Forecast = {
  current: { time: string; temperature_2m: number; weather_code: number };
};

export default async function (input: Input): Promise<Output> {
  const { geo } = input;

  if (!geo) {
    throw new Error("No geolocation provided");
  }

  const url = new URL("https://api.open-meteo.com/v1/forecast");
  url.searchParams.set("latitude", String(geo.latitude));
  url.searchParams.set("longitude", String(geo.longitude));
  url.searchParams.set("current", "temperature_2m,weather_code");

  // A retry re-reads, which is right: the reading is whatever the sky says now, and the
  // process pins nothing about it.
  const res = await fetch(url);
  if (!res.ok) {
    // `name` is what the caller's switch tells one refusal from another by; the status is
    // for whoever reads the failure. eval-node/README.md.
    const err = new Error(`open-meteo answered  ${res.status}`);
    err.name = "UpstreamError";
    throw err;
  }
  const { current } = (await res.json()) as Forecast;

  const condition =
    CONDITIONS[current.weather_code] ?? `wmo ${current.weather_code}`;
  const temperature_c = Math.round(current.temperature_2m * 10) / 10;
  const summary = `${geo.name}  ${current.time}  ${temperature_c} °C  ${condition}`;

  console.log(new Date().toISOString(), summary);

  await appendFile("weather.log", `${new Date().toISOString()} ${summary}\n`);

  return { time: current.time, condition, temperature_c, summary };
}
