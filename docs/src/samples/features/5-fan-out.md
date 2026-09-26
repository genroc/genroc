---
caption: "Fan out over an array. Each child is its own durable instance, and the results come back typed, in input order."
---

```genroc-lsp
name: greet-all
input_schema:
  type: array
  items: { type: string }
tasks:
  - id: greet
    action:
      type: child_list
      name: greet
      over: "$: map(input, (name) => { who: name })"
      result_schema:
        type: object
        properties: { message: { type: string } }
        required: [message]
    output: "$: self.result"
    switch: end
output: "$: outputs.greet"
```
