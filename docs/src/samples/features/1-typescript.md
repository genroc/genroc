---
caption: "A step can be TypeScript. It is bundled and typechecked on apply, against types generated from the task’s own schemas."
---

```genroc
  - id: greet
    action:
      type: child
      <<: "$process: ./script-node.genroc.yaml"
      input:
        code: "$import: ./greet.ts"
        input: { who: "$: input.who" }
      result_schema:
        type: object
        properties:
          greeting:
            type: string
```

```ts
// greet.ts
import type { Input, Output } from "./greet.genroc";

export default (input: Input): Output => {
  return {
    greeting: `hello, ${input.who}`
  }
};
```
