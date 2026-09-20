# Checkout Return Page Templates

Two standalone, mobile-friendly templates for a generic Stripe Checkout flow.
They show a payment outcome and direct the customer to the merchant for order
information. Customize them to suit your application's business flow.

- `success.html`: displays “Payment successful”, reminds the customer not to
  pay again, and suggests checking their order with the merchant.
- `cancel.html`: displays “Payment not completed” and asks the customer to
  check their order before trying again. It does not cancel an order or request
  a refund. A Checkout cancel redirect is not evidence that a charge failed.

Both files include their own CSS, JavaScript, and icons. No build step, external
assets, Stripe.js, API keys, or backend requests are required. They support
English, Simplified Chinese, and Traditional Chinese. The language menu offers
all three choices and remembers manual changes when local storage is available.
The initial language follows the browser's ordered language preferences, using
the first supported language and falling back to English if none is supported.
Without JavaScript, the English content and expandable help still work.

Traditional Chinese uses shared wording suitable for Hong Kong and Taiwan,
including “付款”, “訂單”, and “商戶”. The language menu names languages and
scripts without region labels or flags.

At a fixed viewport size, each text area reserves the space needed by its longest
translation. Switching languages keeps the page, card, and text-area dimensions
stable, including expanded help. Shorter translations can leave extra space.
The layout still adapts to a resized viewport and retains normal page scrolling;
text is not clipped or reduced to fit. Only the active translation is visible
and exposed to assistive technology.

## Deployment

1. Copy both HTML files to a public HTTPS static-file location on your domain,
   for example `/stripe/`. Customers must be able to open them without signing in.
2. Set these fields in the payment channel's driver configuration, replacing the
   example domain with your own:

   ```json
   {
     "SuccessURL": "https://payments.example.com/stripe/success.html",
     "CancelURL": "https://payments.example.com/stripe/cancel.html"
   }
   ```

3. Create a new payment after updating the configuration. Existing Checkout
   Sessions retain the return URLs supplied when they were created.

The URLs can optionally include `?lang=en`, `?lang=zh-Hans`, or `?lang=zh-Hant`
to choose the initial language explicitly; the language menu remains available.
Supported language aliases work in URL parameters and browser preferences:

| Language tag                         | Page language       |
| ------------------------------------ | ------------------- |
| `en`, `en-*`                         | English             |
| `zh-Hant`, `zh-HK`, `zh-TW`, `zh-MO` | Traditional Chinese |
| `zh-Hans`, `zh-CN`, `zh-SG`, `zh`    | Simplified Chinese  |

An explicit `Hans` or `Hant` script takes precedence over the region; for example,
`zh-Hans-HK` selects Simplified Chinese and `zh-Hant-TW` selects Traditional
Chinese. Chinese tags without a script or a recognized Traditional Chinese
region default to Simplified Chinese. Existing `?lang=zh` links and saved `zh`
preferences remain compatible. Detection uses language preferences, not location.

A supported URL parameter takes precedence over a saved preference, which in
turn takes precedence over the browser language preferences.
`{CHECKOUT_SESSION_ID}` is not needed because these pages do not query an order.
Do not put a Stripe secret key or webhook signing secret in either page.

For two isolated live environments, configure each channel with its own return
URLs. Keep the webhook endpoint and its `WebhookSecret` configured separately;
these HTML pages are not webhook handlers.
See the driver's [required webhook subscriptions](../../README.md#webhooks)
for the complete seven-event list, endpoint URL, and signing-secret configuration.

To preview locally, run this command from the repository root:

```sh
python3 -m http.server 8765 --bind 127.0.0.1 --directory examples/checkout-pages
```

Then open <http://127.0.0.1:8765/success.html> or
<http://127.0.0.1:8765/cancel.html> in your browser.

To test the complete Checkout flow, see [Sandbox testing](../../README.md#sandbox-testing)
for successful, declined, and 3D Secure test cards, sample form values, and links
to Stripe's official test card documentation.

## Payment Flow

1. The application creates a payment and presents the Checkout URL as a link,
   redirect, or QR code.
2. The customer opens Stripe Checkout and pays.
3. Stripe redirects the phone to `SuccessURL` after completing Checkout, or to
   `CancelURL` when the customer uses Checkout's back/cancel navigation.
4. Independently, Stripe sends events to the merchant's backend. The application
   uses verified payment records to update the order and fulfill the purchase.

The result headings are presentation copy, not a payment verification mechanism.
Visiting `success.html` does not prove that a payment succeeded. If you enable
asynchronous payment methods, adapt the page to display a pending state until
your backend confirms success; completing Checkout can precede payment success.
Customers may also close their browsers without visiting either page. Continue
using verified backend payment status to decide when to fulfill the order. See
[Stripe's success page guide](https://docs.stripe.com/payments/checkout/custom-success-page?payment-ui=stripe-hosted).

`CancelURL` is the destination for Checkout's back button, not a generic payment
failure handler. Card errors can remain on Checkout so the customer can retry;
closing the browser does not guarantee a redirect. Leaving Checkout does not
call the driver's `CancelPayment` method. See
[Stripe's Checkout Session parameters](https://docs.stripe.com/api/checkout/sessions/create).

If your application needs a definitive “Payment failed” page, display that state
based on a backend-confirmed payment failure rather than a visit to `CancelURL`.

## Customization

Edit the `messages.en`, `messages['zh-Hans']`, and `messages['zh-Hant']` objects
in each HTML file to change the brand name, instructions, or support text. The
default brand is “Payment” / “支付结果” / “付款結果”. Also update the default
English HTML text and `<title>` so the same changes appear when JavaScript is
disabled. Colors and spacing are defined in the inline `<style>` block.

Add your own merchant return link, support details, or next-step instructions as
needed. The templates do not guess a return URL or provide a retry button without
an order-specific destination. Language preferences use the
`stripe-checkout-return-language` local storage key.

The pages deliberately show no order number or amount.
If you later need order-specific information, add a backend endpoint that
validates the request and reads trusted payment records before displaying it.
