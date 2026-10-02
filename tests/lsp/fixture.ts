// One realistic definition exercising every slot the language server answers for. Every fixture
// here must stay VALID (`genctl apply --check-only`), or every answer about it is suspect.

export const ORDERS = `name: orders

input_schema:
  type: object
  properties:
    customer_id: { type: string }
    amount: { type: number }
    currency: { type: string }
  required: [customer_id, amount, currency]

tasks:
  - id: price
    action:
      type: fetch
      url: "https://api.example.com/price?customer=\${ input.customer_id }"
      method: get
      headers:
        X-Currency: "\${ input.currency }"
      responses:
        "200":
          type: object
          properties:
            total: { type: number }
            discount: { type: number }
          required: [total]
    output:
      charged: "$: self.result.total - (self.result.discount ?? 0)"
    switch:
      - case: "self.output.charged > 1000"
        goto: "$review"
      - goto: "$fulfil"
    on_error:
      - code: [http.500]
        retry: { retries: 3, delay: 2s }
        goto: "$review"

  - id: review
    action:
      type: external
      result_schema:
        type: object
        properties:
          approved: { type: boolean }
        required: [approved]
    switch:
      - case: "self.result.approved"
        goto: "$fulfil"
      - goto: end

  - id: fulfil
    action:
      type: child
      name: shipment
      input:
        order: "$: input.customer_id"
    on_error:
      - code: [carrier_down]
        goto: "$review"
    switch: end
`;

// The process `orders` spawns. Its own file, because that is what makes go-to-definition
// across files a real question.
export const SHIPMENT = `name: shipment
tasks:
  - id: dispatch
    action:
      type: fetch
      method: post
      url: "https://api.example.com/ship"
    on_error:
      - code: [http.5%]
        raise: { code: carrier_down, message: "the carrier would not take it" }
    switch: end
`;

// An expression that BINDS a name: a lambda parameter is in scope only inside the body.
export const FANOUT = `name: fanout
input_schema:
  type: object
  properties:
    lines:
      type: array
      items:
        type: object
        properties:
          sku: { type: string }
          qty: { type: integer }
        required: [sku, qty]
  required: [lines]

tasks:
  - id: ship
    action:
      type: child_list
      name: shipment
      over: "$: map(input.lines, (line) => { order: line.sku })"
    switch: end
`;

// What a GUARD proves: every nullable here is reachable, so each narrowing is real, not vacuous.
export const GUARDED = `name: guarded
input_schema:
  type: object
  properties:
    user_id: { type: string }
  required: [user_id]

tasks:
  - id: load
    action:
      type: fetch
      method: get
      url: "https://api.example.com/users/\${ input.user_id }"
      responses:
        200:
          type: array
          items:
            type: object
            properties:
              email: { type: string }
              activated: { type: boolean }
            required: [email, activated]
        404:
          type: object
          properties:
            retry_after: { type: [integer, "null"] }
          required: [retry_after]
    output: "$: self.result[0]"
    on_error:
      - case: "error.data.retry_after == null"
        goto: $failed
      - case: "error.data.retry_after > 0"
        goto: $failed
      - goto: $failed
    switch:
      - case: "self.output == null"
        goto: $missing
      - case: "self.output.activated"
        goto: end
      - goto: $nudge

  - id: nudge
    action:
      type: fetch
      method: post
      url: "https://api.example.com/emails"
      body:
        to: "$: outputs.load.email"
    switch: next

  - id: charge
    action:
      type: fetch
      method: post
      url: "https://api.example.com/charge"
      responses:
        200:
          type: object
          properties:
            receipt: { type: [string, "null"] }
          required: [receipt]
        429:
          type: object
          properties:
            wait: { type: [integer, "null"] }
          required: [wait]
    output:
      receipt: "$: self.result.receipt"
    on_error:
      - code: ["http.429"]
        case: "error.data.wait != null"
        retry:
          retries: 2
          delay: "$: error.data.wait"
      - goto: $failed
    switch:
      - case: "self.output.receipt != null"
        panic:
          code: already_charged
          message: "already \${ self.output.receipt }"
      - goto: end

  - id: missing
    output:
      absent: "$: outputs.load == null"
    switch: end

  - id: failed
    switch: end
`;
