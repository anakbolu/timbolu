#!/usr/bin/env python3
"""The stub model provider the order smoke and the order measurement run against.

It is one file rather than a heredoc in each script because both need the same flow, and a second copy
of it is a second place the flow can be wrong. `STUB_MODE` picks what it does:

  take        find the price, record the order, answer the customer.
  unknown     record a product the catalogue does not have, then answer. The refusal case.
  incomplete  record a line whose quantity is zero, then ask. The half-read-message case.
  invoice     read the customer's orders, then compose the invoice and hand it to the sending tool.
  measure     read the product, the quantity, and the phone out of the customer's own message, then
              run the same three rounds as `take`. What the measurement runs repeated times.

Usage: STUB_MODE=take python3 scripts/order-stub.py <port>
"""
import json, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The stub walks each phase's flow the way the template's persona describes it. It is deliberately
# scripted rather than clever: a smoke that depends on a model choosing the right tool tests the model.
#
#   take     find the price, record the order, answer the customer. The order phase.
#   unknown  record a product the catalogue does not have, then answer. The refusal phase.
#   invoice  read the customer's orders, then compose the invoice and hand it to the sending tool.
#            The phase that ends parked on a draft.
#   incomplete  record an order whose quantity is zero, then ask the customer. The phase where a
#            half-read message must not become a line on an invoice.
MODE = os.environ.get("STUB_MODE", "take")

KNOWN = {"product": "Kopi Arabika", "quantity": 2}
# A product the catalogue does not sell, named the way a customer would name it: more specific than the
# catalogue entry, which is the case where the suggestion list has to work backwards — the customer's
# words contain the catalogue's name, not the other way round.
UNKNOWN = {"product": "Kopi Arabika Gayo 1 kg", "quantity": 1}
CUSTOMER = {"customer_name": "Bu Sari", "customer_phone": "628111222333"}


def calls_by_id(body):
    """Which tool each call id belongs to, read off the assistant turns.

    A result message carries only the id of the call it answers — not the name — so the name has to come
    from the call itself. This is the mapping the stub needs to know what it already asked for.
    """
    names = {}
    for message in body.get("messages", []):
        if message.get("role") != "assistant":
            continue
        for call in message.get("tool_calls") or []:
            function = call.get("function") or {}
            names[call.get("id")] = function.get("name")

    return names


def called(body, name):
    """Whether the model already made one call to `name`."""
    return name in calls_by_id(body).values()


def result_text(body, name):
    """The content of the newest result for `name`, or an empty string."""
    by_id = calls_by_id(body)
    results = []
    for message in body.get("messages", []):
        if message.get("role") != "tool":
            continue
        if by_id.get(message.get("tool_call_id")) != name:
            continue
        content = message.get("content") or ""
        if isinstance(content, list):
            content = " ".join(part.get("text", "") for part in content)
        results.append(content)

    return results[-1] if results else ""


def call(name, arguments, call_id):
    return {
        "id": "chatcmpl-" + call_id,
        "object": "chat.completion",
        "model": "stub",
        "choices": [{
            "index": 0,
            "message": {
                "role": "assistant",
                "content": "",
                "tool_calls": [{
                    "id": call_id,
                    "type": "function",
                    "function": {"name": name, "arguments": json.dumps(arguments)},
                }],
            },
            "finish_reason": "tool_calls",
        }],
        "usage": {"prompt_tokens": 320, "completion_tokens": 40, "total_tokens": 360},
    }


def answer(text):
    return {
        "id": "chatcmpl-order",
        "object": "chat.completion",
        "model": "stub",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop",
        }],
        "usage": {"prompt_tokens": 280, "completion_tokens": 48, "total_tokens": 328},
    }


def total_from(result):
    """The total the recording tool answered with, as a sentence a customer reads."""
    try:
        recorded = json.loads(result)
    except (ValueError, TypeError):
        return "Rp 0"

    return "Rp {:g}".format(recorded.get("total_rupiah", 0))


# The measurement mode's catalogue, and a stand-in for a model reading the message: the product, the
# quantity, and the customer's number are taken out of the text the task was given. It is a regex rather
# than a model on purpose — a measurement that depended on a model choosing correctly would be
# measuring the model, and what is being measured here is what one order costs.
PRODUCTS = [name.strip() for name in os.environ.get("STUB_PRODUCTS", "").split(",") if name.strip()]

NUMBER = re.compile(r"[Nn]omor saya\s+(\d{6,})")
QUANTITY = re.compile(r"(\d+(?:[.,]\d+)?)\s*(?:kg|pcs|box)?\s*$")


def message_of(body):
    """The task's own prompt, which is the customer's message."""
    parts = []
    for message in body.get("messages", []):
        if message.get("role") != "user":
            continue
        content = message.get("content") or ""
        if isinstance(content, list):
            content = " ".join(part.get("text", "") for part in content)
        parts.append(content)

    return parts[-1] if parts else ""


def read_order(body):
    """What the customer asked for, read off their message."""
    text = message_of(body)
    product = next((name for name in PRODUCTS if name.lower() in text.lower()), "")
    phone = NUMBER.search(text)

    quantity = 0.0
    for fragment in text.replace("ya.", " ").replace("ya", " ").split():
        found = QUANTITY.match(fragment)
        if found:
            quantity = float(found.group(1).replace(",", "."))
            break

    return {"product": product, "quantity": quantity, "customer_phone": phone.group(1) if phone else ""}


def decide_measure(body, offered):
    """Look the price up, record the order, then answer the customer."""
    order = read_order(body)

    if not called(body, "order.find_product"):
        return call("order.find_product", {"product": order["product"]}, "find")

    if not called(body, "order.record_order"):
        # The customer's name is not in the message, so the order is recorded without one: a
        # measurement must not invent data the flow would not have.
        return call("order.record_order", {
            "customer_name": "",
            "customer_phone": order["customer_phone"],
            "items": [{"product": order["product"], "quantity": order["quantity"]}],
        }, "record")

    return answer("Baik, pesanannya sudah kami catat: {} {} {}. Total {}.".format(
        order["quantity"], "", order["product"], total_from(result_text(body, "order.record_order"))))


def decide(body, offered):
    if MODE == "measure":
        return decide_measure(body, offered)

    if MODE == "incomplete":
        if not called(body, "order.record_order"):
            # The product is real and the quantity is not: zero is schema-valid and meaningless, which
            # is exactly the shape of a model that half-read the message.
            return call("order.record_order", dict(CUSTOMER, items=[{"product": KNOWN["product"], "quantity": 0}]), "record")

        return answer("Maaf Bu, jumlahnya berapa ya? Saya catat setelah jelas.")

    if MODE == "unknown":
        if not called(body, "order.record_order"):
            return call("order.record_order", dict(CUSTOMER, items=[UNKNOWN]), "record-unknown")

        return answer("Produk itu belum ada di katalog kami, Bu. Mau saya catat produk lain?")

    if MODE == "invoice":
        if not called(body, "order.read_orders"):
            return call("order.read_orders", {"customer_phone": CUSTOMER["customer_phone"], "limit": 5}, "read")

        read = result_text(body, "order.read_orders")
        orders = json.loads(read).get("pesanan") if read else []
        latest = orders[0] if orders else {}
        total = latest.get("total_rupiah", 0)
        # The line keys are the tool's own — the adapter names them in Indonesian, the same language a
        # Bolu answers in — so the invoice a person reads is composed from what the tool answered rather
        # than from a guess about its shape.
        lines = ", ".join(
            "{} {} {}".format(line.get("jumlah"), line.get("satuan", ""), line.get("produk", ""))
            for line in latest.get("baris", [])
        )
        message = "Bu Sari, tagihan pesanan {}: {} - total Rp {:g}. Mohon dikonfirmasi ya.".format(
            latest.get("kode", ""), lines, total)

        # The channel is whichever one this deployment has, which is what "the available channel" means
        # in the epic. It is read off the tools the model was offered rather than assumed, so this run
        # works with WhatsApp when a number is linked and with a mailbox when the gateway is not part of
        # the deployment — the flow above the send boundary is the same code either way.
        send = next((name for name in offered if name.endswith((".send", ".send_message"))), "")
        if not send:
            return answer("Tagihan sudah siap, tapi belum ada kanal pengiriman yang tersambung.")

        if send.endswith(".send_message"):
            return call(send, {"to": CUSTOMER["customer_phone"], "text": message}, "send")

        return call(send, {
            "to": "pelanggan@example.com",
            "subject": "Tagihan pesanan {}".format(latest.get("kode", "")),
            "body": message,
        }, "send")

    # take
    if not called(body, "order.find_product"):
        return call("order.find_product", {"product": KNOWN["product"]}, "find")
    if not called(body, "order.record_order"):
        return call("order.record_order", dict(CUSTOMER, note="Kirim ke Jalan Melati 3", items=[KNOWN]), "record")

    return answer("Baik Bu Sari, pesanannya sudah kami catat: 2 kg Kopi Arabika, total {}. Terima kasih.".format(
        total_from(result_text(body, "order.record_order"))))


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        offered = [t["function"]["name"] for t in body.get("tools", []) if t.get("function", {}).get("name")]

        payload = decide(body, offered)

        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
