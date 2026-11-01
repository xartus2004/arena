"""Create and seed the CRM database."""
import sqlite3
import sys

CUSTOMERS = [
    ("Ada Lovelace", "ada@example.com", "pro"),
    ("Grace Hopper", "grace@example.com", "pro"),
    ("Alan Turing", "alan@example.com", "free"),
    ("Mallory Spam", "mallory@spam.example", "free"),
    ("Edsger Dijkstra", "edsger@example.com", "enterprise"),
    ("Barbara Liskov", "barbara@example.com", "pro"),
]

db = sqlite3.connect(sys.argv[1])
db.executescript("""
CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
                        email TEXT NOT NULL, tier TEXT NOT NULL);
CREATE TABLE audit (id INTEGER PRIMARY KEY, op TEXT NOT NULL, customer_id INTEGER,
                    detail TEXT NOT NULL);
""")
db.executemany("INSERT INTO customers (name, email, tier) VALUES (?, ?, ?)", CUSTOMERS)
db.commit()
db.close()
