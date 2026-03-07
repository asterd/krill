# Krill use cases

# 1. OpenClaw like agent-loop and chat capabilities
L'utente può connettere il proprio telegram/whatsapp/slack/discord a krill e cominciare una conversazione durante la quale può fare richieste al motore di vario tipo, da chiedere il meteo, a verificare dati su un certo sistema, a riassumere documenti, ricercare su internet o sviluppare codice. Il prompt può essere sia testuale che vocale e deve essere multimodale. E' necessario che il risultato sia paragonabile alle capability di openClaw in tal senso.

# 2. Invoke custom skills
L'utente, tramite chiamata puntuale o tramite chat può richiedere che venga eseguita una skill particolare che deve avere la possibilità di fare una serie di attività quali:
- connettersi ad un db ed eseguire una query
- autenticarsi ad un servizio e richiamare una API
- eseguire attività complesse varie. 
La skill può dare esempi di codice da eseguire specificamente per portare a termine la missione.

# 3. Scheduling activities
L'utente può chiedere o con api o in modalità conversazionale l'esecuzione schedulata (puntuale o ripetitiva) di una skill o un set complesso di attività

# 4. A2UI
L'utente può sviluppare un'app che si interfaccia con agenti, funzionalità varie, etc, generando l'interfaccia tramite protocollo A2UI.

# 5. Orchestrazione multi-agent autonoma
L'utente può programmare una verticalizzazione di agenti con compiti specifici che possa interoperare autonomamente tra di loro per raggiungere un obiettivo. Esempio se volessi fare un e-commerce che si gestisce da solo, potrei skillare N agenti ognuno con alcune caratteristiche e questi potrebbero cooperare tra di loro per manutenere ed evolvere l'e-commerce, avendo però cura di chiedere all'utente tramite canali chat notificati come intervenire o se fornire dati aggiuntivi. Quindi capacità di creare piani di esecuzione lineari o a grafo, poter eseguire agenti in sequenza o parallelamente, eseguire flussi pre-costituiti o saperli costruire on-the-fly con controllo costi ed esecuzione dettagliato.

# 6. Creazione asset operativi
L'utente può richiedere la creazione di un'applicativo web ed il suo hosting su servizi terzi specificati in skill per una fruizione istantanea
