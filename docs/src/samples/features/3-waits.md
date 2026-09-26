---
caption: "Waits last minutes or months — this one loops weekly, forever. The instance sits in the database, not in a thread, so restarts and deploys do not touch it."
---

```genroc-lsp
name: weekly-digest
tasks:
  - id: wait_for_monday
    action:
      type: delay
      until: "mon 09:00"
      tz: Europe/Prague
    switch: next

  - id: send
    action:
      type: fetch
      method: post
      url: "https://api.example/digest"
    switch: $wait_for_monday
```
