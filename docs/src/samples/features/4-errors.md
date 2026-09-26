---
caption: "Retries back off with jitter. Anything else becomes a named error the caller can match on."
---

```genroc-lsp
name: charge
tasks:
  - id: charge
    action:
      type: fetch
      method: post
      url: "https://pay.example/charges"
    on_error:
      - code: [http.503]
        retry:
          retries: 5
          delay: 1s
          max_delay: 5m
        
      - code: [http.402]
        raise:
          code: card_declined
          message: "the card was declined"
    switch: end
```
