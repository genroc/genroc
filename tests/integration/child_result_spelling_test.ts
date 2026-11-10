import { expect, test } from "vitest";
import { client, runToEnd } from "../helpers/client.ts";

// A result_schema says the same thing however its union is spelled, and registration and collect's
// conform must agree on it: the child below hands back `n: null`.

/** Declares `n: integer|null` and leaves it null when `skip` is set. */
function child(name: string) {
  return {
    name,
    input_schema: { type: "object", properties: { skip: { type: "boolean" } }, required: ["skip"] },
    tasks: [
      { id: "start", switch: [{ case: "input.skip", goto: "end" }, { goto: "next" }] },
      { id: "t", output: "$: 1", switch: "end" },
    ],
    output_schema: {
      type: "object",
      properties: { n: { type: ["integer", "null"] } },
      required: ["n"],
    },
    output: { n: "$: outputs.t" },
  };
}

const SPELLINGS: Record<string, object> = {
  "a type list": { type: ["integer", "null"] },
  "an anyOf": { anyOf: [{ type: "integer" }, { type: "null" }] },
  "an anyOf with a $ref arm": { anyOf: [{ $ref: "#/$defs/Count" }, { type: "null" }] },
};

for (const [spelling, n] of Object.entries(SPELLINGS)) {
  for (const type of ["child", "child_list"] as const) {
    test(`${type}: a nullable result declared as ${spelling} registers and collects the null`, async () => {
      const uid = crypto.randomUUID().slice(0, 8);
      const childName = `spell_child_${uid}`;
      const parent = `spell_parent_${uid}`;
      const resultSchema = { type: "object", properties: { n }, required: ["n"] };
      const action =
        type === "child"
          ? { type, name: childName, input: { skip: true }, result_schema: resultSchema }
          : { type, name: childName, over: "$: [{skip: true}]", result_schema: resultSchema };

      const { error } = await client.PUT("/definitions", {
        body: { definitions: [child(childName), {
          name: parent,
          $defs: { Count: { type: "integer" } },
          tasks: [{ id: "a", action, output: "$: self.result", switch: "end" }],
          output: "$: outputs.a",
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
        }] as any },
      });
      expect(error, `${spelling} describes exactly the child's integer|null`).toBeUndefined();

      const { status, data } = await runToEnd(parent);
      expect(status, "collect's conform must accept what registration admitted").toBe("completed");
      expect(data?.output).toEqual(type === "child" ? { n: null } : [{ n: null }]);
    });
  }
}
