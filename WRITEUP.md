# Movie Ticket Booking System
### Tech stack - Go, Postgres, Docker

## What I would do?
I did not use Redis, as the task encourages using only one database. I would have used it for quicker checking of the state of the seats: if they are held, the request could be denied directly without checking the DB. If all requests go to the database, they will occupy the connection pool for the same record on the same table, but all of them will be waiting because one request already has a lock. Not just that, this could occupy all the connections with requests that are not very beneficial, when those connections could be used for other, more necessary requests. Redis can also keep a value for a certain time and then delete it, which is the perfect use case for changing a seat from held to available. Redis can also be used to keep data on the consecutive seats that are available.

The positioning of the seats is not considered: how far a seat is from the screen, where the aisles are, and how far left or right it is. The app only knows seat names, so A5 and A6 count as side by side even if an aisle runs between them, and a suggestion can be anywhere in the hall, not near the seats the user picked. In future I would store each seat's distance from the screen, the aisle breaks and its left/right position, so suggestions can avoid splitting a group across an aisle, stay close to the seats the user chose, keep a split group in neighbouring rows, and prefer better seats (centre and mid-hall over the front corners).

## How it works
It uses time-bound seat reservations with lazy expiration. It records a time until which the reservation is valid; if that time is before now(), the seat is considered available for re-reservation by any other user or the same user. I did not have clarity on when to confirm the reservation, so I confirm it immediately. In production I assume the payment gateway would confirm its status, for us to change it from "held" to "confirmed". It would change to held once a user clicks "Pay now". It will remain held for 10 minutes, giving the user time to re-attempt the payment if it fails. The app only gets a final response of payment failed or succeeded, so we can allow the user to retry the payment if it fails within the time period, and even extend the valid time a few times. Not more than a few times, because that would give bad actors a chance to block seats from being booked indefinitely, by repeatedly delaying the payment so the seats stay reserved for them while they never pay.

## Using the best-effort model if only part of a seat selection is available
If a user selects seats that are only partially available, the request is declined with a 409 and the response includes `suggested_seats`: the longest available consecutive seats for the requested number of seats. If those are also short, it concatenates the top longest available runs of consecutive seats until the requested number of seats is reached. These runs of consecutive seats may be separated geographically, but they give the least amount of segregation for a group who would like to sit together for a movie. If a longer sequence is available, but there is also a sequence of exactly the requested size, it gives the exact-size sequence instead of taking a portion of the longest one.

The suggested seats are not held, so another user can book them before this user retries; the retry is then declined with a fresh suggestion.


> used Opus 5.5 in Claude Code
> since I had to declare how I used AI for this project, I decided to document my approach and use it as the prompt to the LLM
## Here is the prompt I wrote
The full prompt is in [docs/ai-prompt.md](docs/ai-prompt.md).

## After the prompt
I asked it to test the APIs with all possible combinations. It found 3 bugs in the code.

1. When a user did not exist at login, it returned 500 instead of 401.
2. A bearer token in lower case was rejected, even though the HTTP spec says it should be case-insensitive.
3. A big price was causing an overflow because it was too big to hold inside an int64. The solution: if a show's price * limit_per_user is more than can fit in the variable, it is rejected with a 409.



After load testing, the AI agent made these changes: on the reserve endpoint, an unexpected field such as `user_id` in the body is ignored, because identity always comes from the token and a `user_id` in the body represents a spoofed user. The other endpoints reject unexpected fields with a 400.
It found there was indeed a need for a cancel endpoint, which I had not provided to it earlier, due to the ambiguity of the task description. It was mentioned as "cancel endpoint or time-bound".

There were some refused requests due to Docker's Windows port forwarding. It was not due to the app: when the testing was done from within the Docker network, all requests were accepted. This was 4k requests, but 20k requests got 1.7k drops, which was due to
"the 5-second ReadHeaderTimeout expires before saturated goroutines get scheduled, and Go reuses that same value as the keep-alive idle timeout by default."
So what was happening was that by the time these goroutines got their chance to execute, the time to read the headers was over, because other requests were occupying the resources. Once the timeout has happened, the goroutines cannot read the headers, so those requests got dropped; that's how Go behaves. So the solution was to increase the timeout from 5s to 30s.



## Atomic decision and deadlock
we did not check in a query if a sat is available and then in the next query update it to held else it leaves room for another questy to alos update the same set between 2 queries, hence we made it atomic, by using one query to check if it is available and then update it else do nothing at all.

## Idempotency
the key is the primary key, ensures that no dupliacate booking.
If key is already used then does nothing

## AI usage and Decisions
All decisions were made by me unless specified as decisions made by AI. All errors and changes to fix them, Observability, Metrics, Logs, Burst Test were all were done by AI.

## Consistency vs Availibiilty
Here being consistent is more important thatn the server being available, double booking, user spoofing, double payment are all things we absolutely cannot be tolerated.

## Observability
The app exposes /metrics (request rate and latency per route, bookings confirmed/declined/cancelled by reason, seat counts per show) and writes JSON logs where every line carries a request_id, also returned in the X-Request-ID header, so a single failing request can be traced.