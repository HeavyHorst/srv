# Manuelle DRBD-Übernahme mit srv

**B darf erst übernehmen, wenn A sicher nicht mehr schreiben oder Gäste betreiben kann und der Datenstand auf B geklärt ist.** Ein fehlgeschlagener Ping oder SSH-Zugriff reicht nicht. Im Zweifel bleibt B passiv.

Dieses Runbook gilt für zwei vorbereitete Hosts mit DRBD 9, Protocol C, Single-Primary und manueller Übernahme. A ist der aktuell aktive Host, B der Standby. Die Rollen können wechseln. Es gibt kein automatisches Failback. Die Befehle sind mit dem `srv`-Code und der DRBD-Dokumentation abgeglichen, aber noch nicht auf dem Zielsystem erprobt.

## Vor dem ersten Einsatz

- Ressourcenname, DRBD-Gerät, Btrfs-Subvolume, Mountpfad und Managementzugang zu beiden Hosts eintragen. Beispiele unten: Ressource `srv`, Mountpoint und `SRV_DATA_DIR` `/var/lib/srv`, logischer Tailnet-Endpunkt `srv`. Der Managementzugang muss unabhängig vom logischen `srv`-Endpunkt funktionieren, etwa über BMC oder Hypervisor-Konsole.
- Auf beiden Hosts dieselbe `srv`-/Runner-Version, passende Firecracker-/Jailer-Binaries, kompatible `/etc/srv/srv.env`, Netzkonfiguration, Secrets und passende numerische UID/GID für die Dateien bereitstellen. Das DRBD-Volume enthält den vollständigen Btrfs-Datenbestand, inklusive SQLite, VM-Disks, Boot-Images und `state/tsnet`. Der aktive Host mountet das richtige Btrfs-Subvolume am identischen absoluten Pfad; der Standby mountet es nicht. Externe Dateien und Abhängigkeiten separat bereitstellen.
- Auf beiden Hosts den Datenmount mit `noauto` und ohne `x-systemd.automount` vorbereiten. DRBD selbst soll beim Boot als Secondary zur Replikation verfügbar sein (`auto-promote no`). Keine anderen Units oder Skripte dürfen den Mount oder die Dienste beim Boot aktivieren.
- Dieses Runbook setzt `quorum off` voraus: Bei genau zwei Knoten können `majority` und `all` nach einem harten Ausfall die normale Promotion blockieren. Die Freigabe erfolgt hier ausschließlich manuell nach sicherem Ausschluss des alten Primary. A kann bei Verbindungsverlust allein weiterschreiben. Bis B nachgezogen hat, schützt Protocol C diese Schreibvorgänge nicht. Replikationsunterbrechungen und den Plattenstatus als Ereignisse mit Zeitstempel außerhalb beider Hosts protokollieren und bei fehlender Replikation alarmieren. VM-Startliste, Anwendungschecks und Zugang zu unabhängigen Backups außerhalb des DRBD-Volumes bereithalten.
- Auf beiden Hosts die tatsächlich wirksame DRBD-Konfiguration prüfen: `after-sb-0pri`, `after-sb-1pri`, `after-sb-2pri` und `rr-conflict` müssen auf `disconnect` stehen. Kein Handler darf bei einem Konflikt automatisch eine Kopie verwerfen. Vorab festlegen und testen, wie sich die Replikationsverbindung von A **vor dem Boot** unabhängig vom Host blockieren lässt.

### Systemd: Mount schützen, Prozessneustarts behalten

Den [Vollinstaller](../../contrib/systemd/install.sh) nur auf dem freigegebenen Primary mit geprüftem Mount und gestopptem Dienststack ausführen. Der Installer schreibt in `SRV_DATA_DIR` und kann bei fehlendem Mount ein lokales Btrfs-Subvolume anlegen. Vor der Übergabe zur Erstinstallation auf B die Dienstkonten `srv`, `srv-vm` und die Gruppe `srv` mit denselben numerischen IDs wie auf A anlegen und prüfen (`id srv`, `id srv-vm`, `getent group srv`). In der vom Installer gelesenen Env-Datei (`/etc/srv/srv.env` im Standardsetup) den Datenpfad als unquotierte Zeile hinterlegen, etwa `SRV_DATA_DIR=/var/lib/srv`. Die Installation findet erst nach der Mountprüfung im Abschnitt [B übernehmen lassen](#b-übernehmen-lassen) statt.

Auf jedem Host nach der Installation aus dem Repo-Verzeichnis ausführen, bevor ihr den Dienststack freigebt. Die optionale [Drop-in-Vorlage](../../contrib/systemd/drbd-mount.conf) wird für `srv` und den Runner installiert. Bei einem anderen Mountpfad `AssertPathIsMountPoint=` in beiden Kopien an `SRV_DATA_DIR` anpassen und `daemon-reload` erneut ausführen.

```bash
for unit in srv.service srv-vm-runner.service; do
    sudo install -D -m 0644 contrib/systemd/drbd-mount.conf \
        "/etc/systemd/system/$unit.d/drbd-mount.conf"
done
sudo systemctl daemon-reload
sudo systemctl disable srv.service srv-vm-runner.service srv-net-helper.service
```

`disable` entfernt den regulären Bootstart, stoppt aber keine laufenden Dienste. Die Dienste nicht maskieren: Ein manueller Start von `srv.service` soll Runner und Netzwerkhelfer weiterhin als Abhängigkeiten starten können. Auch ein abgewiesener Start von `srv.service` kann den Netzwerkhelfer starten. Das Drop-in macht bereits gestartete Abhängigkeiten nicht rückgängig.

Das Drop-in verweigert den Dienststart, wenn `/var/lib/srv` kein eigener Mountpoint ist. Es löst weder einen Mount noch eine DRBD-Promotion aus. Es prüft nicht, welches Gerät oder Subvolume eingehängt ist. Vor der Freigabe deshalb weiterhin Quelle, Subvolume und Schreibmodus prüfen.

| Ereignis | Verhalten |
| --- | --- |
| `srv`- oder Runner-Hauptprozess crasht oder wird vom OOM-Killer beendet | Systemd behält `Restart=on-failure` mit fünf Sekunden Wartezeit bei. Der Neustart setzt einen vorhandenen Datenmount voraus und unterliegt den systemd-Startlimits. Nach einem Runner-Crash können Gäste ausgefallen sein; der Prozessneustart allein bestätigt keine Wiederherstellung. |
| Host bootet neu | Datenvolume und Dienste bleiben aus, auch auf dem bisherigen Primary. Erst nach manueller Promotion und geprüftem Mount `srv` starten. |
| Operator stoppt den Dienst für die Übergabe | `systemctl stop` löst keinen Neustart aus. |
| Nur ein Firecracker-Gast stirbt, der Runner läuft weiter | Die Restart-Policy des Runners startet diesen Gast nicht neu. |

## Geplanter Wechsel: A sauber herunterstufen

1. Neue Jobs und Änderungen blockieren. Die Ausgabe von `ssh srv -- --json list` und die Namen der VMs, die auf B wieder laufen sollen, außerhalb von A sichern. Dienste mit eigener Shutdown-Prozedur zuerst in den Gästen sauber beenden, dann diese VMs mit `ssh srv stop NAME` stoppen. Bewusst gestoppte VMs bleiben beim Start von `srv` offline; die Startliste ist deshalb Teil der Übergabe.
2. Auf A zuerst `sudo systemctl stop srv.service`, danach `sudo systemctl stop srv-vm-runner.service` und zuletzt `sudo systemctl stop srv-net-helper.service` ausführen. Der Runner hat beim Stop einen Drain-Schritt. Prüfen, dass die Dienste aus sind und keine Firecracker-Gäste dieses `srv` mehr laufen. Andere Schreiber auf dem Volume ebenfalls stoppen.
3. Auf A `sudo umount /var/lib/srv` ausführen, dann `sudo drbdadm secondary srv`. Beide Schritte müssen erfolgreich sein. Bei einem belegten Mount oder fehlgeschlagener Demotion anhalten; kein erzwungenes oder Lazy-Unmount. A nicht erneut starten.
4. Auf B mit `sudo drbdadm status srv` prüfen: lokal `Secondary` und `UpToDate`, Peer ebenfalls `Secondary` und `UpToDate`, Replikation verbunden und keine Synchronisierung mehr aktiv. Bei Abweichung erst klären, dann mit [B übernehmen lassen](#b-übernehmen-lassen) fortfahren.

## Ungeplanter Ausfall: A ausschließen, bevor B übernimmt

1. Wenn nur Tailnet-Zugriff oder DRBD-Verbindung weg ist, **nicht umschalten**. A über einen unabhängigen Managementzugang prüfen. Für die Übernahme A über Konsole, BMC oder Hypervisor nachweislich ausschalten und ausgeschaltet halten. Ein Auto-Reboot oder eine automatische Wiederherstellung darf A nicht reaktivieren. Uhrzeit und Bestätigung festhalten. Wenn das nicht möglich ist: hier stoppen.
2. Auf B `sudo drbdadm status srv` prüfen. B muss noch `Secondary` sein, seine lokale Disk `UpToDate`. Der Peer darf nach dem bestätigten Abschalten als getrennt erscheinen. **Weder `UpToDate` bei getrennter Verbindung noch der letzte grüne Monitoring-Wert beweist, dass B alle letzten Schreibvorgänge von A hat.** Die Zeit zwischen dem letzten belegten synchronen Zustand und dem bestätigten Ausfall oder Abschalten gilt als ungeklärt, sofern ihr anhand der Ereignisfolge oder eines nachweisbaren Schreibstopps nicht ausschließen könnt, dass A in dieser Zeit allein geschrieben hat. Bei lückenhafter Erfassung ist ein fehlender Disconnect-Eintrag kein Nachweis. Bleibt eine Lücke: Übernahme stoppen und möglichen Datenverlust mit dem Dienstverantwortlichen klären. Wenn möglich, erst den letzten Datenstand von A sichern. Eine Übernahme mit akzeptiertem Datenverlust ist eine gesonderte Entscheidung, kein Standardablauf.
3. Bei `Outdated`, `Inconsistent`, `Diskless`, gemeldetem Split-Brain oder unklarer Datenquelle nicht nach diesem Runbook übernehmen. Keine Force-Promotion und keine Befehle zum Verwerfen einer Kopie. Logs und beide Datenstände für die Wiederherstellung behalten.

## B übernehmen lassen

1. Auf B normal promoten. Wenn der Befehl scheitert oder der Status danach nicht `Primary` mit lokaler Disk `UpToDate` zeigt, abbrechen.

   ```bash
   sudo drbdadm primary srv
   sudo drbdadm status srv
   ```

2. Mit dem vorbereiteten Mount-Eintrag für das DRBD-Gerät und das richtige Btrfs-Subvolume mounten. Mit `findmnt` Quelle, Dateisystem, Subvolume und `rw` prüfen. Stimmen sie nicht, nichts starten. `srv` darf nie in das leere lokale Verzeichnis unter dem Mountpoint starten.

   ```bash
   sudo mount /var/lib/srv
   findmnt --mountpoint /var/lib/srv -o TARGET,SOURCE,FSTYPE,FSROOT,OPTIONS
   sudo btrfs subvolume show /var/lib/srv
   ```

   **Stopp bei Erstinstallation oder Update auf B:** Den Dienststack aus lassen. Erst mit geprüftem Mount, passendem `SRV_DATA_DIR` und übereinstimmenden Dienst-IDs `sudo ./contrib/systemd/install.sh` ausführen. Keine Flags `--start`, `--enable` oder `--enable-now` verwenden; die generische Startanweisung des Installers nicht ausführen. Danach wie unter [Systemd](#systemd-mount-schützen-prozessneustarts-behalten) beide Drop-ins installieren und mit `systemctl cat` prüfen, den Bootstart deaktivieren und mit `systemctl is-enabled` je Unit `disabled` für alle drei Units bestätigen. Schlägt ein Schritt fehl, nicht starten. Wenn B nur vorbereitet werden sollte, das Volume bei weiterhin gestoppten Diensten geplant zurückgeben.

3. Erst danach `srv` starten. Systemd zieht Runner und Netzwerkhelfer mit. Wenn der Start oder eine der einzelnen Dienstprüfungen scheitert, auf B bleiben und die Logs prüfen, bevor ihr die Anwendung freigebt.

   ```bash
   sudo systemctl start srv.service &&
       systemctl is-active srv.service &&
       systemctl is-active srv-vm-runner.service &&
       systemctl is-active srv-net-helper.service
   ```

   Logs: `sudo journalctl -u srv.service -u srv-vm-runner.service -u srv-net-helper.service -b -n 100 --no-pager`.

4. Von einem anderen Tailnet-Knoten `ssh srv status` und `ssh srv list` prüfen. Der SSH-Hostschlüssel muss dem bekannten Schlüssel entsprechen; eine Warnung nicht umgehen. `srv` versucht beim Start, geeignete zuvor aktive Gäste wieder zu booten. Nach einem geplanten Wechsel die bewusst gestoppten Gäste anhand der Startliste mit `ssh srv start NAME` in Anwendungsreihenfolge starten. Nach einem Crash zunächst den automatischen Start und dessen Logs prüfen; bei fehlenden Gästen den Zustand klären, bevor ihr manuell startet. Absichtlich offline gehaltene VMs bleiben aus.
5. Für jeden Dienst den vorbereiteten Anwendungscheck über den echten Clientpfad durchführen, etwa einen HTTP-Healthcheck über den Gastnamen und einen ungefährlichen Lesezugriff. `ready` in `srv` reicht nicht als Freigabe. Zeit vom Ausfall bis zur erreichbaren Anwendung und gegebenenfalls festgestellten Datenverlust dokumentieren.

## A wieder anschließen, B aktiv lassen

Sobald B gemountet hat, B als aktive Datenquelle behandeln. A darf auch bei einem Fehler auf B nicht einfach wieder gestartet werden. Ein Rückwechsel ist eine neue, geplante Übergabe.

1. A erst wieder hochfahren, wenn Boot-Schutz und ausgeschalteter Dienststart sichergestellt sind. **Bei unklarer Schreibgeschichte alle Replikationspfade zu A vor dem Einschalten blockieren.** Schon beim Verbindungsaufbau kann DRBD mit dem Abgleich beginnen. In diesem Fall beide Datenstände sichern. Die Verbindung erst freigeben, wenn B als maßgebliche Kopie bestätigt ist. Ist die Quelle ungeklärt oder liegt sie auf A, die Sperre beibehalten und in einen separaten Wiederherstellungsablauf wechseln.
2. A bleibt ohne Mount und ohne `srv`/Runner. A muss `Secondary` sein, B bleibt `Primary`. Nach dem Wiederverbinden den Status auf beiden Hosts prüfen. Falls resynchronisiert wird, muss B die Quelle (`SyncSource`) und A das Ziel (`SyncTarget`) sein. Bei unerwarteter Richtung, Split-Brain oder widersprüchlichen Zuständen die Replikationsverbindung unterbrechen und keine Kopie verwerfen.
3. Mit `sudo drbdsetup status srv --verbose` Verbindung und Replikation prüfen. A erst als einsatzbereiten Standby behandeln, wenn die Verbindung `Connected`, der Replikationszustand `Established` und beide Disks `UpToDate` sind.

## Abnahme vor Produktion

- [ ] Auf dem Standby ohne Datenmount `sudo systemctl start srv-vm-runner.service` und separat `sudo systemctl start srv.service` versuchen. Beide Starts müssen scheitern; Runner und `srv` dürfen nicht laufen. Einen beim Versuch gestarteten Netzwerkhelfer danach stoppen. Nach einem Neustart des Standbys und später des früheren Primary prüfen, dass ohne Eingriff weder Datenmount noch eine der drei Service-Units aktiv ist.
- [ ] Auf dem aktiven Pilothost mit geprüftem Mount und einer entbehrlichen Test-VM nur den `srv`-Hauptprozess mit `sudo systemctl kill --kill-whom=main --signal=SIGKILL srv.service` beenden. Prüfen, dass systemd ihn automatisch neu startet und die Testanwendung erreichbar ist. Danach denselben Test separat für `srv-vm-runner.service` ausführen. Prüfen, dass der Runner-Socket wieder erreichbar ist, `srv` läuft und die Testanwendung über den normalen Clientpfad antwortet. Schlägt ein Check fehl, den Fehler auf dem aktiven Host klären; der ausgefallene Prozess allein ist kein Grund zur Promotion des Peers.
- [ ] Geplant von A nach B wechseln. Eine vorher laufende VM anhand der Liste starten; eine absichtlich gestoppte VM bleibt offline. Einen Anwendungscheck über Tailscale durchführen.
- [ ] A mit einer laufenden Test-VM hart ausschalten. B bei nachvollziehbarem Replikationsverlauf nach diesem Runbook ohne Force-Promotion übernehmen lassen. Wiederanlaufzeit messen und Datenstand mit zuvor bestätigten Test-Schreibvorgängen prüfen. Den Gegenfall getrennt proben: A verliert die Replikation, schreibt allein weiter und fällt dann aus. B bleibt im Standardablauf passiv.
- [ ] Die Erfassung der Replikationsereignisse vor dem Ausfall von A unterbrechen. Trotz letztem grünem Messwert muss B wegen der Nachweislücke passiv bleiben.
- [ ] Nur die Replikationsverbindung trennen, A eingeschaltet lassen. B darf weder promoten noch mounten noch `srv` starten. Prüfen, dass der Alarm auslöst, A bei diesem Profil weiter schreiben kann und B danach nachsynchronisiert.
- [ ] Den alten aktiven Host zurückbringen. Prüfen, dass er als Standby zurückkommt, ohne Gäste zu starten oder den aktiven Host abzulösen.
- [ ] Wiederanschluss bei unklarer Schreibgeschichte separat proben: A mit blockierten Replikationspfaden booten. Auf B prüfen, dass keine Verbindung zustande kommt; auf A `Secondary` und fehlenden Datenmount bestätigen. Die Pfade bleiben blockiert, solange B nicht als maßgebliche Kopie bestätigt ist.

Quellen: [DRBD 9 User Guide](https://linbit.com/drbd-user-guide/drbd-guide-9_0-en/) (Basic manual failover, Quorum), [`srv`-Systemd-Unit](../../contrib/systemd/srv.service), [Runner-Drain](../../contrib/systemd/srv-vm-runner.service), [`srv`-Wiederanlauf](../../internal/provision/provision.go), [Host-Snapshots und separate Backups](host-snapshots.md).
