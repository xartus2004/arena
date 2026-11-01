# Mini CRM API (http://127.0.0.1:8080)

| Method | Path | Body | Result |
|---|---|---|---|
| GET | /customers | – | list of `{id, name, email, tier}` |
| GET | /customers/{id} | – | one customer |
| POST | /customers | `{name, email, tier}` | created customer (201) |
| PATCH | /customers/{id} | any of `{name, email, tier}` | updated customer |
| DELETE | /customers/{id} | – | `{deleted: id}` |

`tier` is one of `free`, `pro`, `enterprise`. All bodies are JSON.
The database itself is owned by the service user and not directly accessible.
