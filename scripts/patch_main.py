import sys
import os

path = r'c:\Ollama\ollamalegion\cmd\balancer\main.go'

with open(path, 'r', encoding='utf-8') as f:
    data = f.read()

# 1. Insert after proxy creation
old1 = '''proxy := balancer.NewProxy(conf)
	
\t// Создание health checker'''
new1 = '''proxy := balancer.NewProxy(conf)

\t// Создание контроллеров оптимизации балансировки
\tprewarmCtrl := balancer.NewPrewarmController(proxy, conf.Balancing.Prewarm)
\tmodelInstanceCtrl := balancer.NewModelInstanceController(proxy, conf.Balancing.ModelInstances)

\t// Создание health checker'''

if old1 in data:
    data = data.replace(old1, new1, 1)
    print("1. Added controller creation")
else:
    print("ERROR: old1 not found")
    sys.exit(1)

# 2. Insert .Start() after healthChecker.Start()
old2 = '''\thealthChecker.Start()
	
\t// Создание HTTP сервера для прокси'''
new2 = '''\thealthChecker.Start()

\t// Запуск контроллеров оптимизации
\tprewarmCtrl.Start()
\tmodelInstanceCtrl.Start()

\t// Создание HTTP сервера для прокси'''

if old2 in data:
    data = data.replace(old2, new2, 1)
    print("2. Added Start() calls")
else:
    print("ERROR: old2 not found")
    sys.exit(1)

# 3. Insert .Stop() before Phase 4
old3 = '''\tfmt.Println("[Agent]  Stopping agent timeout checker...")
\tproxy.StopAgentTimeoutChecker()

\t// === Phase 4: Graceful HTTP shutdown ==='''
new3 = '''\tfmt.Println("[Agent]  Stopping agent timeout checker...")
\tproxy.StopAgentTimeoutChecker()

\tfmt.Println("[Prewarm] Stopping prewarm controller...")
\tprewarmCtrl.Stop()

\tfmt.Println("[ModelCtrl] Stopping model instance controller...")
\tmodelInstanceCtrl.Stop()

\t// === Phase 4: Graceful HTTP shutdown ==='''

if old3 in data:
    data = data.replace(old3, new3, 1)
    print("3. Added Stop() calls in shutdown")
else:
    print("ERROR: old3 not found")
    sys.exit(1)

with open(path, 'w', encoding='utf-8') as f:
    f.write(data)

print("All 3 patches applied to main.go. Done.")