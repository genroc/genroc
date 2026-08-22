import { Input, Output } from "./location.genroc";

export default async function (input: Input): Promise<Output> {
  const url = new URL("https://geocoding-api.open-meteo.com/v1/search");
  url.searchParams.append("name", input.place);
  url.searchParams.append("count", "1");
  url.searchParams.append("format", "json");

  const response = await (await fetch(url)).json();

  return response.results[0];
}
