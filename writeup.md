# Movie Ticket Booking System
### Techstack - Go, Postgres, Docker

## what I would do?
Did not use redis as the task encourages to use only one database. I would have used it for quicker checking of state of the seats if they are held, then could directly deny the request without checking the db. if all go to the database they will occupy the connection pool for the same record on the same table but all the requests will be waiting because on request already has a lock. Not just that, this could occupy all the connections with not very benifitial requests, the connection can be used for other more necessary requests. Redis can also keep a value for a certain time and then delete the value, this will be the perfect use case to change the value from held to available. Redis can also be used keep a data of consequent seats available. 

## How it works
using timebound sear reservation expiry using lazy expiration. It registers a time uptil when the reservation is valid, if the time is before now(), then it is considered as available for re-reservation by any other or same user. I did not have clarity on when to confirm the reservation, so I immediately confirm it. In production I assume this will be used for the payment gateway to confirm it's status for us to change it from "held" to "confirmed". It would change to held once a user clicks pay now. It will remain held for 10mins giving user to re atttempt payment if it had failed. The app only gets a final response of payment failed or success, so we can allow the user to retry the payment if it fails in the time period and even increase valid time for a few times, not more than a few time because it will give a chance for bad actors to infinitely block some seats from getting booked by keeping on delaying the payment and the seats would stay reserved for them and they never make the payment.

## using the best effort model if a selection of seat is available
If a user selects a seat which is partially available, will return a response of the list of longest available consequent seats for the requested amount of seats. If they are also short, will concatenate the list of the top longest available consequent seates upto the requested amount of seats are provided. These list of consequent seats will be will be seperate away grographically but will give the list amount of segregation of a group who would like to sit together for a movie. If a longer sequence is available, but there is also a sequence exactly for the size of the requested amount of seats, then give the exact size sequence instead of selecting a portion from the longest available.


> used opus 5.5 in claude code
> since I had to declare how I used AI for this project, decided to document my approach and use it as the prompt to the LLM
## Here is the prompt I wrote

make a users table
- user_id // primary key // any unique username
- hashed_password

make a user_sessions table
- session_id // primary key
- user_id // foreign key
- refresh_token // unique
- expires_at // timestamp

make an endpoint for creating a user (/user/register)
- user_id // if already exists decline with a 409
- password

hash the password before storing in db, salt_rounds available in env file
only validation is email should be unique, if success return 200 and user_id

make an endpoint for login a user (/user/login)
- user_id
- password 

// hash the inputed password and check if same as the db // if user not found or incorrect passord return respective message
// create a jwt token and pass user id as payload and expiry time for 15 mins, take jwt_secret from env file
// return the user name and jwt_token in the response with 200 OK

Also, make an api doc for anyoen to test the backend.


Make a table for shows
- show_id // primary key
- name
- limit_per_user // defult 4 // max no. of seats a user can book
- price_paise // price per seat

Make a seats table
- seat_id
- name // A1, A2, A3 ...
- show_id // foreign key

Make a show_seats table (show_id and seat_id together will be unique)
- show_id // foreign key
- seat_id // foreign key

make an endpoint to create a show (unauthenticated)
1. Create a show — POST /shows (admin)
{ "name": "friday-night", "seats": ["A1","A2","A3","..."], "price_paise": 25000 }
Returns the created show with an id and every seat in "available" state.

Make a table for reservations
- reservation_id (idempotency_key) // primary kay
- seats (array of strings, json type) //validation for this if seats are not under reserved timelimit or available
- user_id // foreign key // who booked the seat 
- show_id // foreign key
- status ("available" || "held" || "confirmed") // default - available
- valid_upto (timestamp) // use validation_timeout from env file to check if the reservation is longer than validation_timeout time


2. Reserve a seat — POST /shows/{id}/reserve (authenticated user)
- Identity comes from the auth token, not from the request body.
- The request names the seat(s) wanted and carries an idempotency key (header or body):
{ "seats": ["A12"], "idempotency_key": "…" }
Success 201:
{ "reservation_id": "…", "show_id": "…", "user_id": "…", "seats": ["A12"], "amount_paise": 25000, "status": "confirmed" }


- when this request is made it should set the seats to "held" if valid_upto timestamp less than now or the table does not have a record (lazy expiry), and then immediately set them to "confirmed" (the response status is "confirmed"). This operation needs to be in one query. if they are 2 seperate queries, then conflicts between requests can happen between the queries. No double-sell: a seat confirmed (or actively held) for one user can never be confirmed for another. A race for the same seat produces exactly one winner; losers get a clean decline (409), never a 500.
- the number of seats should not exceed the max limit of seats (limit_per_user) for that show.
- when a request is made to seat of a show but the id is already used, then if the number of seats are exactly same with the same user then its a retry and the response should the reservation, if the request body (seats) is different, returns a rejected with error status of 409
- Partial requests: if a user asks for ["A12","A13"] and only one is free, then return a response of the list of longest available consequent seats for the requested amount of seats. If they are also short, will concatenate the list of the top longest available consequent seates upto the requested amount of seats are provided. These list of consequent seats will be will be seperate away grographically but will give the list amount of segregation of a group who would like to sit together for a movie. If a longer sequence is available, but there is also a sequence exactly for the size of the requested amount of seats, then give the exact size sequence instead of selecting a portion from the longest available.


4. Show state — GET /shows/{id}
Returns per-seat status (available / held / confirmed) and counts. available + held + confirmed == total_seats must hold at all times — this is your reconciliation invariant.






## After the prompt
I asked it to test the apis with all posible combiantions. It found 3 bugs in the code.

1. When a user did not exist for login, it would return 500 instead of 401
2. bearer token in lower case was rejected, even though HTTP spec says it isshould be case-inssensitive.
3. big price was causeing overflow because it was too big to hold inside int64, so the solution is if any show with a price * per_seat_limit is more that which can fit in the variable, then it is rejected with a 409.  



After load testing, the AI Agent made these changes, if an unexpected field is found, the request is dropped and no response. it representa a spoofed user.
Found there was indeed a need of a cancel endpoint which i and not provided to it earlied, due to the ambiguity of the task description. It was mentioned as cancel endpoint or timebound.

There was some refusal of requests due to docker's windows port forwarding. It was not due to the app. when the testing was done from within the docker network, all requests were accepted. This was 4k requests but 20k requests got 1.7k drops which was due to 
"the 5-second ReadHeaderTimeout expires before saturated goroutines get scheduled, and Go reuses that same value as the keep-alive idle timeout by default." 
So what was happening was that by the time these go routines got their chance to execute, the time to check the header was over because of other request occuping the resources. Once the timeout has happened, the go routines cannot check the headers, hence those requests got dropped out, that's how go behaves. So the solution ws to increase the timer from 5s to 30 s
























# Main Title
## Section Title
### Subsection Title

**Bold text**
*Italic text*
`Inline code`

- Bullet point
- Another point
  - Nested point

1. Numbered list
2. Second item

```sql
CREATE TABLE users (
  id SERIAL PRIMARY KEY,
  name VARCHAR(100),
  email VARCHAR(100) UNIQUE
);

---

## 5. Tables
```markdown
| Table | Purpose |
|-------|---------|
| users | Stores user info |
| events | Stores event details |
| seats | Tracks seat availability |

> Use transactions with `SELECT ... FOR UPDATE` to avoid race conditions.


# 🛠 Backend Solution: Seat Booking

## Schema
```sql
CREATE TABLE reservations (
  id SERIAL PRIMARY KEY,
  user_id INT REFERENCES users(id),
  seat_id INT REFERENCES seats(id),
  reserved_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);


