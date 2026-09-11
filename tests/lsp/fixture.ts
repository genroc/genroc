// The process these tests read. One realistic definition, exercising every slot the language
// server has an answer for: a typed input, a fetch whose url and headers interpolate, a typed
// response, an output that computes from it, a switch that routes on that output, an on_error
// with a retry policy, an external task, and a child that spawns another process — whose raise
// is what that child task catches, one file away.
//
// It is VALID — `genctl apply --check-only` accepts it. A fixture that did not register would
// make every "what can I write here" answer suspect.

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
        retry: { attempts: 3, delay: 2s }
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
