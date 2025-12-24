# 🚀 Wazuh Docker – Multi-Node Deployment

---

## ⚠️ NOTE – System Recommendation

To ensure **best performance and stability**, it is strongly recommended to run this stack on:

| Resource | Recommended |
|----------|-------------|
| 💽 Disk   | **SSD (Highly recommended)** |
| 🧠 CPU    | **Minimum 6 cores** |
| 🧮 RAM    | **Minimum 8 GB** |
| 📦 Storage| **At least 50 GB free space** |

> ❗ Running on HDD or low-resource systems may result in:
> - Slow indexing
> - High latency in dashboard
> - Service crashes
> - OpenSearch instability

---

## ⚙️ Installation

### 1️⃣ Clone the repository
```bash
git clone https://github.com/CyberSecN00bers/Capstone_Blue_Stack.git
cd Blueteam_Stack
```

---

### 2️⃣ Create the environment file
```bash
copy .env.example .env
```

---

### 3️⃣ Initialize nginx submodule
```bash
git submodule update --init --recursive
```

---

### 4️⃣ Generate certificates for Wazuh Indexer cluster
```bash
docker compose -f generate-indexer-certs.yml run --rm generator
```

---

## 🔧 Environment Configuration

Open the `.env` file and configure these **two required variables**:

---

Replace `YOUR_PUBLIC_IP` with your server’s IP address:

```env
CORS_ORIGIN="http://localhost:8080,http://localhost:5173,http://YOUR_PUBLIC_IP:8080"
VITE_API_URL=http://YOUR_PUBLIC_IP:3001/api
```

---

## ▶️ Start the Stack

### Run in background
```bash
docker compose up -d
```

⏱ First launch may take about **1 minute** while Wazuh initializes indexes.

---

## 🌐 Access

Open your browser:
Wazuh-dashboard:

```
https://HOST_IP:444
```
Waf-dashboard:

```
http://HOST_IP:8080
```
---
---

## 🧹 Clean Up

To stop and remove everything:

```bash
docker compose down -v
```

---

## 📝 Notes

- If your host IP changes, update `.env` and restart:
  ```bash
  docker compose down
  docker compose up -d
  ```
- If the dashboard is unreachable:
  ```bash
  docker logs wazuh.dashboard
  docker logs wazuh.master
  ```

---

## ⭐ Credits

Wazuh · Docker · OpenSearch · Nginx

---

Happy SecOps! 🔥
