// Zapisovací server pro proces `weather-logger` (playground/process.yaml).
//
//   bun run playground:weather
//
// POST /record {place}   — place je jméno města, "Praha" nebo "Brno"
//   → přeloží jméno na souřadnice (open-meteo geocoding, bez API klíče, cachované),
//   → přečte aktuální počasí,
//   → připíše jeden řádek do weather.log,
//   → vrátí { time, temperature_c }.
//
// Neznámé město je 404, ne 500: proces to dostane jako `http.404` a umí ho odlišit od
// výpadku, který má cenu zkoušet znovu.
//
// Kdy zavolat další tick server neřeší — proces má `until: "*:*:00"`, takže si další
// celou minutu spočítá genroc sám.

import { appendFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join } from "node:path";

const PORT = Number(process.env.WEATHER_PORT ?? 3002);
const LOG = process.env.WEATHER_LOG ?? join(import.meta.dirname, "weather.log");

interface RecordRequest {
  place: string;
}

// Jméno města, které geocoding nezná. Nese se až k 404.
class UnknownPlace extends Error {}

// WMO weather codes, jen ty běžné — cokoliv jiného se vypíše číslem.
const conditions: Record<number, string> = {
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

// Jméno města → souřadnice. Cachované, protože pro daný název je to konstanta a proces
// se ptá každou minutu na to samé.
const located = new Map<string, { lat: number; lon: number; label: string }>();

async function locate(place: string) {
  const hit = located.get(place);
  if (hit) return hit;

  const url =
    `https://geocoding-api.open-meteo.com/v1/search?count=1&format=json` +
    `&name=${encodeURIComponent(place)}`;

  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`geocoding ${res.status}: ${(await res.text()).slice(0, 200)}`);
  }
  const body = (await res.json()) as {
    results?: { latitude: number; longitude: number; name: string; country?: string }[];
  };
  const top = body.results?.[0];
  if (!top) throw new UnknownPlace(`neznámé město: ${place}`);

  const found = {
    lat: top.latitude,
    lon: top.longitude,
    label: top.country ? `${top.name}, ${top.country}` : top.name,
  };
  located.set(place, found);
  console.log(`  ${place} → ${found.label} (${found.lat}, ${found.lon})`);
  return found;
}

async function readWeather(lat: number, lon: number) {
  const url =
    `https://api.open-meteo.com/v1/forecast?latitude=${lat}&longitude=${lon}` +
    `&current=temperature_2m,relative_humidity_2m,wind_speed_10m,weather_code`;

  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`open-meteo ${res.status}: ${(await res.text()).slice(0, 200)}`);
  }
  const body = (await res.json()) as {
    current: {
      temperature_2m: number;
      relative_humidity_2m: number;
      wind_speed_10m: number;
      weather_code: number;
    };
  };
  return body.current;
}

const server = createServer(async (req, res) => {
  const path = new URL(req.url ?? "/", "http://localhost").pathname;
  if (path !== "/record") {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: `no handler for ${path}` }));
    return;
  }

  let raw = "";
  for await (const chunk of req) raw += chunk;

  try {
    const { place } = JSON.parse(raw) as RecordRequest;
    const where = await locate(place);
    const current = await readWeather(where.lat, where.lon);

    const now = new Date();
    const condition =
      conditions[current.weather_code] ?? `wmo ${current.weather_code}`;
    const line =
      `${now.toISOString()}\t${where.label}\t` +
      `${current.temperature_2m} °C\t${current.relative_humidity_2m} %\t` +
      `${current.wind_speed_10m} km/h\t${condition}\n`;

    await appendFile(LOG, line);
    console.log(`← /record ${line.trimEnd()}`);

    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(
      JSON.stringify({
        time: now.toISOString(),
        temperature_c: current.temperature_2m,
      }),
    );
  } catch (err) {
    // 404 znamená "tohle se opakováním nespraví"; cokoliv jiného je výpadek.
    const status = err instanceof UnknownPlace ? 404 : 500;
    console.log(`← /record [${status}] ${err}`);
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: String(err) }));
  }
});

server.listen(PORT, () => {
  console.log(`Weather server listening on http://localhost:${PORT}`);
  console.log(`Appending to ${LOG}`);
});
