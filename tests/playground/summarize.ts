// The summarize task's script. `Input` is the type genroc INFERRED for what process.yaml
// passes; `Output` is what its result_schema declares. Both are generated — run
// `genctl types -f script.yaml -f process.yaml`, or just apply.
//
// Template literals need no escaping here: genctl doubles every `$` on splice, so `${…}`
// is JavaScript rather than a genroc interpolation. That is the whole point of the import.
import type { Input, Output } from "./summarize.genroc";

// WMO weather codes, jen ty běžné — cokoliv jiného se vypíše číslem.
const CONDITIONS: Record<number, string> = {
  0: "jasno", 1: "skoro jasno", 2: "polojasno", 3: "zataženo",
  45: "mlha", 48: "namrzající mlha",
  51: "mrholení", 53: "mrholení", 55: "silné mrholení",
  61: "slabý déšť", 63: "déšť", 65: "silný déšť",
  71: "slabé sněžení", 73: "sněžení", 75: "husté sněžení",
  80: "přeháňky", 81: "přeháňky", 82: "silné přeháňky",
  95: "bouřka", 96: "bouřka s kroupami", 99: "bouřka s kroupami",
};

export default async function (input: Input): Promise<Output> {
  const { reading, geo } = input;

  const condition = CONDITIONS[reading.code] ?? `wmo ${reading.code}`;
  const temperature_c = Math.round(reading.temperature_c * 10) / 10;

  return {
    condition,
    temperature_c,
    summary: `${geo.label}  ${reading.time}  ${temperature_c} °C  ${condition}`,
  };
}
