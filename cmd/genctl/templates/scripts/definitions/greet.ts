// `Input` and `Output` are written beside this file by `genctl types`, and by every apply.
import type { Input, Output } from "./greet.genroc";

export default function (input: Input): Output {
  return { greeting: `hello, ${input.who}`, at: new Date().toISOString() };
}
