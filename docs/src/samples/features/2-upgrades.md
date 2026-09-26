---
caption: "Running instances can move to a new version. Each one’s state is checked against the new definition, and the ones that would not fit are refused."
---

```text
> genctl upgrade onboard --from 1 --to 2
3kjfx4vm   onboard      REFUSED  at "wait": outputs: required property "email" is missing
02pcayvt   onboard      -> 2 (1 in tree)

moved 1 tree(s) from 1 to 2, 1 refused
```
