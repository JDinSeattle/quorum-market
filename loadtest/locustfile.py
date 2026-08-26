"""Load profile for the Quorum Market system.

Traffic goes through the gateway, which is the only publicly reachable service
and therefore the only honest thing to measure. That means every session
registers an account and carries a token, exactly as a real client would, and
the numbers include the cost of verifying it.

The shape of the traffic matters more than its volume. Real storefronts are
overwhelmingly browse traffic: shoppers look at many products, add a few to a
cart, and most leave without buying anything. A test that checked out every
session would put the expensive distributed path — reserve, authorize, commit,
publish — under load it never sees in production, while leaving the read path
under-tested.

The distributions come from the project's frequency assumptions:

    browses before each action   log-normal(mu=1.3, sigma=0.1)   mode ~3.6
    add-to-cart actions/session  log-normal(mu=1.1, sigma=0.15)  mode ~3
    checkout rate                10% (90% abandon)

Run it against the gateway:

    locust -f loadtest/locustfile.py --host http://localhost:8080
    locust -f loadtest/locustfile.py --host http://<alb-dns> --headless \\
        --users 50 --spawn-rate 5 --run-time 5m
"""

import math
import random
import string

from locust import HttpUser, SequentialTaskSet, between, events, task

# The catalogue seeded by cmd/productloader: p1..p1000.
PRODUCT_COUNT = 1000

PASSWORD = "load-test-password-not-a-secret"

# Outcomes that are part of normal operation rather than defects. A declined
# card and an out-of-stock item are the system working correctly, so counting
# them as failures would make a healthy run look broken.
EXPECTED = {
    "add-item": {400, 404, 409},
    "checkout": {402, 409},
}

# Reported at the end so business outcomes stay visible even though they are
# not failures.
outcomes = {
    "sessions": 0,
    "checkouts": 0,
    "declined": 0,
    "out_of_stock": 0,
    "abandoned": 0,
    "throttled": 0,
}


def log_normal_int(mu: float, sigma: float, low: int = 1, high: int = 100) -> int:
    """Draw a log-normal integer clamped to [low, high]."""
    return max(low, min(int(round(math.exp(random.gauss(mu, sigma)))), high))


def random_product() -> str:
    return f"p{random.randint(1, PRODUCT_COUNT)}"


def random_email() -> str:
    handle = "".join(random.choices(string.ascii_lowercase + string.digits, k=12))
    return f"load-{handle}@example.com"


def random_card() -> str:
    """A well-formed 16 digit number, so declines come from the authorizer's
    own 10% rate rather than from validation."""
    return "-".join("".join(random.choices(string.digits, k=4)) for _ in range(4))


class Shopper(HttpUser):
    """One simulated customer, with one account, running sessions in a loop."""

    # Think time between sessions. Without it every simulated user would hammer
    # the system in a tight loop, which measures the client as much as the
    # server.
    wait_time = between(1, 3)

    def on_start(self):
        """Register once, then reuse the account for every session.

        Registration is deliberately outside the session loop: password hashing
        is expensive by design, and re-registering per session would turn this
        into a benchmark of bcrypt rather than of the shop.
        """
        self.customer_id = None
        self.token = None
        self.refresh_token = None
        self.cart_id = None

        with self.client.post(
            "/identity/register",
            json={"email": random_email(), "password": PASSWORD},
            name="/identity/register",
            catch_response=True,
        ) as response:
            if response.status_code in (200, 201):
                body = response.json()
                self.token = body["accessToken"]
                self.refresh_token = body.get("refreshToken")
                self.customer_id = body["profile"]["customerId"]
                response.success()
            elif response.status_code == 429:
                outcomes["throttled"] += 1
                response.success()
            else:
                response.failure(f"could not register: {response.status_code}")

    def auth(self) -> dict:
        return {"Authorization": f"Bearer {self.token}"} if self.token else {}

    def refresh_session(self) -> bool:
        """Exchange the refresh token for a new pair.

        Access tokens are short-lived on purpose, so a run longer than their
        lifetime has to refresh — and exercising that path is worth doing,
        since it is what every real client does.
        """
        if not self.refresh_token:
            return False

        with self.client.post(
            "/identity/refresh",
            json={"refreshToken": self.refresh_token},
            name="/identity/refresh",
            catch_response=True,
        ) as response:
            if response.status_code == 200:
                body = response.json()
                self.token = body["accessToken"]
                self.refresh_token = body.get("refreshToken")
                response.success()
                return True
            response.success()  # an expired session is not a server fault
            return False

    tasks = []  # populated below, after ShoppingSession is defined


class ShoppingSession(SequentialTaskSet):
    """Browse, add a few things, then usually leave."""

    def on_start(self):
        self.items_in_cart = 0
        self.planned_adds = log_normal_int(1.1, 0.15, low=1, high=10)
        outcomes["sessions"] += 1

        if not self.user.token:
            self.interrupt()

    def browse(self):
        """Look at a handful of products before doing anything else.

        Unauthenticated on purpose: browsing is public, and most storefront
        traffic has no session behind it.
        """
        for _ in range(log_normal_int(1.3, 0.1, low=1, high=10)):
            with self.client.get(
                f"/product/{random_product()}",
                name="/product/{productId}",
                catch_response=True,
            ) as response:
                if response.status_code == 429:
                    outcomes["throttled"] += 1
                response.success() if response.status_code in (200, 404, 429) \
                    else response.failure(f"browse failed: {response.status_code}")

    @task
    def open_cart(self):
        self.browse()

        with self.client.post(
            "/shopping-cart",
            json={},
            headers=self.user.auth(),
            name="/shopping-cart [create]",
            catch_response=True,
        ) as response:
            if response.status_code in (200, 201):
                self.user.cart_id = response.json().get("cartId")
                response.success()
            elif response.status_code == 401 and self.user.refresh_session():
                response.success()
                self.interrupt()
            elif response.status_code == 429:
                outcomes["throttled"] += 1
                response.success()
                self.interrupt()
            else:
                response.failure(f"could not create a cart: {response.status_code}")
                self.interrupt()

    @task
    def fill_cart(self):
        if not self.user.cart_id:
            self.interrupt()

        for _ in range(self.planned_adds):
            self.browse()

            with self.client.post(
                f"/shopping-cart/{self.user.cart_id}/add-item",
                json={
                    "productId": random_product(),
                    "quantity": log_normal_int(0.5, 0.6, low=1, high=5),
                },
                headers=self.user.auth(),
                name="/shopping-cart/{cartId}/add-item",
                catch_response=True,
            ) as response:
                if response.status_code == 200:
                    self.items_in_cart += 1
                    response.success()
                elif response.status_code in EXPECTED["add-item"]:
                    response.success()
                elif response.status_code == 429:
                    outcomes["throttled"] += 1
                    response.success()
                else:
                    response.failure(f"add-item failed: {response.status_code}")

    @task
    def review_orders(self):
        """Look at previous orders, the way a returning shopper does.

        Ordered before the checkout decision deliberately: 90% of sessions
        abandon and interrupt there, so anything after it would almost never
        run. This is also the only load the order service gets, and the only
        thing that exercises a prefix scan across a read quorum rather than
        the point reads everything else does.
        """
        with self.client.get(
            "/orders?limit=10",
            headers=self.user.auth(),
            name="/orders",
            catch_response=True,
        ) as response:
            if response.status_code in (200, 401, 429):
                response.success()
            else:
                response.failure(f"order history failed: {response.status_code}")

    @task
    def checkout_or_leave(self):
        if not self.user.cart_id or self.items_in_cart == 0:
            outcomes["abandoned"] += 1
            self.interrupt()

        # One last look around before deciding.
        self.browse()

        if random.random() < 0.9:
            outcomes["abandoned"] += 1
            self.interrupt()

        with self.client.post(
            f"/shopping-cart/{self.user.cart_id}/checkout",
            json={"creditCard": random_card()},
            headers={
                **self.user.auth(),
                # A key per attempt, so a retry inside Locust would replay
                # rather than buy the cart twice.
                "Idempotency-Key": f"load-{self.user.customer_id}-{random.randint(1, 10**9)}",
            },
            name="/shopping-cart/{cartId}/checkout",
            catch_response=True,
        ) as response:
            if response.status_code == 200:
                outcomes["checkouts"] += 1
                response.success()
            elif response.status_code == 402:
                outcomes["declined"] += 1
                response.success()
            elif response.status_code == 409:
                outcomes["out_of_stock"] += 1
                response.success()
            elif response.status_code == 429:
                outcomes["throttled"] += 1
                response.success()
            else:
                response.failure(f"checkout failed: {response.status_code}")

        self.interrupt()


Shopper.tasks = [ShoppingSession]


@events.quitting.add_listener
def report_outcomes(environment, **_kwargs):
    attempted = outcomes["checkouts"] + outcomes["declined"] + outcomes["out_of_stock"]
    sessions = max(outcomes["sessions"], 1)

    print("\n── session outcomes ─────────────────────────────")
    print(f"  sessions          {outcomes['sessions']}")
    print(f"  abandoned carts   {outcomes['abandoned']}")
    print(f"  checkouts placed  {outcomes['checkouts']}")
    print(f"  cards declined    {outcomes['declined']}")
    print(f"  out of stock      {outcomes['out_of_stock']}")
    print(f"  throttled         {outcomes['throttled']}")
    print(f"  conversion rate   {100 * attempted / sessions:.1f}% attempted checkout")
    print("─────────────────────────────────────────────────\n")
