# The prompt I wrote

This is the prompt I gave Opus 5.5 in Claude Code to build the first version, kept exactly as written. See [WRITEUP.md](../WRITEUP.md#after-the-prompt) for what happened after it.

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
