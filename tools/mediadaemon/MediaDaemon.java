import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.PrintWriter;
import java.lang.reflect.Method;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.util.ArrayList;
import java.util.List;
import android.os.IBinder;
import android.os.SystemClock;
import android.media.session.PlaybackState;
import android.media.MediaMetadata;

public class MediaDaemon {
    private static final int PORT = 4545;

    public static void main(String[] args) {
        System.out.println("MediaDaemon starting on port " + PORT + "...");
        try (ServerSocket serverSocket = new ServerSocket(PORT, 10, InetAddress.getByName("0.0.0.0"))) {
            System.out.println("MediaDaemon listening on 0.0.0.0:" + PORT);
            while (true) {
                try {
                    Socket clientSocket = serverSocket.accept();
                    Thread t = new Thread(new Runnable() {
                        @Override
                        public void run() {
                            handleClient(clientSocket);
                        }
                    });
                    t.start();
                } catch (Exception e) {
                    System.err.println("Accept error: " + e.getMessage());
                }
            }
        } catch (Exception e) {
            e.printStackTrace();
        }
    }

    private static void handleClient(Socket socket) {
        try (
            BufferedReader in = new BufferedReader(new InputStreamReader(socket.getInputStream()));
            PrintWriter out = new PrintWriter(socket.getOutputStream(), true)
        ) {
            String line;
            while ((line = in.readLine()) != null) {
                line = line.trim();
                if (line.isEmpty()) continue;
                if ("quit".equalsIgnoreCase(line) || "exit".equalsIgnoreCase(line)) break;
                if ("ping".equalsIgnoreCase(line)) {
                    out.println("PONG");
                    continue;
                }

                String response = executeCommand(line);
                out.println(response);
            }
        } catch (Exception e) {
            System.err.println("Client handler error: " + e.getMessage());
        } finally {
            try { socket.close(); } catch (Exception ignored) {}
        }
    }

    private static class SessionEntry {
        Object controller;
        String pkg;
        PlaybackState pbState;
        int state;
        long livePos;

        boolean isReleaseSmartTube() {
            return "org.smarttube.stable".equals(pkg) || "com.teamsmart.videomanager.tv".equals(pkg);
        }

        boolean isBetaSmartTube() {
            return "org.smarttube.beta".equals(pkg);
        }

        boolean isSmartTube() {
            return isReleaseSmartTube() || isBetaSmartTube() || (pkg != null && pkg.contains("smarttube"));
        }
    }

    private static synchronized String executeCommand(String input) {
        String[] parts = input.split("\\s+");
        String cmd = parts[0];
        String arg = parts.length > 1 ? parts[1] : "";
        String pkgPattern = parts.length > 2 ? parts[2].toLowerCase() : "";

        try {
            Class<?> sm = Class.forName("android.os.ServiceManager");
            Method getService = sm.getMethod("getService", String.class);
            IBinder binder = (IBinder) getService.invoke(null, "media_session");

            Class<?> ismStub = Class.forName("android.media.session.ISessionManager$Stub");
            Method asInterface = ismStub.getMethod("asInterface", IBinder.class);
            Object manager = asInterface.invoke(null, binder);

            Method getSessions = manager.getClass().getMethod("getSessions", android.content.ComponentName.class, int.class);
            List<?> binders = (List<?>) getSessions.invoke(manager, null, 0);

            if (binders == null || binders.isEmpty()) {
                return "ERR:NO_SESSIONS";
            }

            Class<?> iscStub = Class.forName("android.media.session.ISessionController$Stub");
            Method asController = iscStub.getMethod("asInterface", IBinder.class);

            List<SessionEntry> entries = new ArrayList<>();
            for (Object b : binders) {
                Object controller = asController.invoke(null, (IBinder) b);
                Method getPkg = controller.getClass().getMethod("getPackageName");
                String pkg = (String) getPkg.invoke(controller);

                SessionEntry entry = new SessionEntry();
                entry.controller = controller;
                entry.pkg = pkg;

                try {
                    Method getPlaybackStateMethod = controller.getClass().getMethod("getPlaybackState");
                    PlaybackState pbState = (PlaybackState) getPlaybackStateMethod.invoke(controller);
                    entry.pbState = pbState;
                    if (pbState != null) {
                        entry.state = pbState.getState();
                        entry.livePos = pbState.getPosition();
                        if (entry.state == PlaybackState.STATE_PLAYING) {
                            long diff = SystemClock.elapsedRealtime() - pbState.getLastPositionUpdateTime();
                            if (diff > 0) {
                                entry.livePos += (long) (diff * pbState.getPlaybackSpeed());
                            }
                        }
                    }
                } catch (Exception ignored) {}

                entries.add(entry);
            }

            SessionEntry target = selectTargetSession(entries, pkgPattern);
            if (target == null) {
                return "ERR:PKG_NOT_FOUND:" + pkgPattern;
            }

            return handleController(target, cmd, arg);
        } catch (Exception e) {
            return "ERR:" + e.getClass().getSimpleName() + ":" + e.getMessage();
        }
    }

    private static SessionEntry selectTargetSession(List<SessionEntry> entries, String pattern) {
        if (pattern == null || pattern.isEmpty() || "any".equals(pattern) || "active".equals(pattern)) {
            // First check playing sessions with release priority
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PLAYING && e.isReleaseSmartTube()) return e;
            }
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PLAYING && e.isBetaSmartTube()) return e;
            }
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PLAYING) return e;
            }
            // Next check paused sessions
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PAUSED && e.isReleaseSmartTube()) return e;
            }
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PAUSED && e.isBetaSmartTube()) return e;
            }
            for (SessionEntry e : entries) {
                if (e.state == PlaybackState.STATE_PAUSED) return e;
            }
            return entries.isEmpty() ? null : entries.get(0);
        }

        if ("smarttube".equals(pattern) || "youtube".equals(pattern)) {
            // Release priority
            for (SessionEntry e : entries) {
                if (e.isReleaseSmartTube()) return e;
            }
            for (SessionEntry e : entries) {
                if (e.isSmartTube()) return e;
            }
            return null;
        }

        // Generic pattern matching
        for (SessionEntry e : entries) {
            if (e.pkg != null && e.pkg.toLowerCase().contains(pattern)) {
                return e;
            }
        }
        return null;
    }

    private static String handleController(SessionEntry entry, String cmd, String arg) throws Exception {
        Object controller = entry.controller;
        String pkg = entry.pkg;
        int state = entry.state;
        long livePos = entry.livePos;

        if ("get".equalsIgnoreCase(cmd) || "status".equalsIgnoreCase(cmd)) {
            long duration = -1;
            String title = "";
            try {
                Method getMetadataMethod = controller.getClass().getMethod("getMetadata");
                MediaMetadata metadata = (MediaMetadata) getMetadataMethod.invoke(controller);
                if (metadata != null) {
                    duration = metadata.getLong(MediaMetadata.METADATA_KEY_DURATION);
                    title = metadata.getString(MediaMetadata.METADATA_KEY_TITLE);
                }
            } catch (Exception ignored) {}

            return "STATUS:" + pkg + ":state=" + state + ":pos=" + livePos + ":duration=" + duration + ":title=" + (title == null ? "" : title.replace(":", " "));
        }

        if ("pause".equalsIgnoreCase(cmd)) {
            invokeTransport(controller, pkg, "pause");
            return "PAUSE_OK:" + pkg;
        }

        if ("play".equalsIgnoreCase(cmd) || "resume".equalsIgnoreCase(cmd)) {
            invokeTransport(controller, pkg, "play");
            return "PLAY_OK:" + pkg;
        }

        if ("next".equalsIgnoreCase(cmd)) {
            invokeTransport(controller, pkg, "next");
            return "NEXT_OK:" + pkg;
        }

        if ("prev".equalsIgnoreCase(cmd) || "previous".equalsIgnoreCase(cmd)) {
            invokeTransport(controller, pkg, "previous");
            return "PREV_OK:" + pkg;
        }

        if ("seek".equalsIgnoreCase(cmd)) {
            long targetMs;
            long multiplier = 1;

            String cleanArg = arg.toLowerCase();
            if (cleanArg.endsWith("s")) {
                multiplier = 1000;
                cleanArg = cleanArg.substring(0, cleanArg.length() - 1);
            }

            if (cleanArg.startsWith("+")) {
                long delta = Long.parseLong(cleanArg.substring(1)) * multiplier;
                targetMs = livePos + delta;
            } else if (cleanArg.startsWith("-")) {
                long delta = Long.parseLong(cleanArg.substring(1)) * multiplier;
                targetMs = Math.max(0, livePos - delta);
            } else {
                targetMs = Math.max(0, Long.parseLong(cleanArg) * multiplier);
            }

            invokeSeek(controller, pkg, targetMs);
            return "SEEK_OK:" + pkg + ":from=" + livePos + ":to=" + targetMs;
        }

        return "ERR:UNKNOWN_COMMAND:" + cmd;
    }

    private static void invokeTransport(Object controller, String pkg, String action) throws Exception {
        Method actionMethod = null;
        for (Method m : controller.getClass().getMethods()) {
            if (m.getName().equals(action)) {
                actionMethod = m;
                break;
            }
        }
        if (actionMethod == null) {
            throw new NoSuchMethodException("No method " + action);
        }
        Class<?>[] paramTypes = actionMethod.getParameterTypes();
        if (paramTypes.length == 0) {
            actionMethod.invoke(controller);
        } else if (paramTypes.length == 1 && paramTypes[0] == String.class) {
            actionMethod.invoke(controller, pkg);
        } else if (paramTypes.length == 2 && paramTypes[0] == String.class) {
            actionMethod.invoke(controller, pkg, null);
        } else {
            throw new RuntimeException("Unknown params for " + action + ": " + java.util.Arrays.toString(paramTypes));
        }
    }

    private static void invokeSeek(Object controller, String pkg, long posMs) throws Exception {
        Method seekMethod = null;
        for (Method m : controller.getClass().getMethods()) {
            if (m.getName().equals("seekTo")) {
                seekMethod = m;
                break;
            }
        }
        if (seekMethod == null) {
            throw new NoSuchMethodException("No seekTo method");
        }
        Class<?>[] paramTypes = seekMethod.getParameterTypes();
        if (paramTypes.length == 1 && paramTypes[0] == long.class) {
            seekMethod.invoke(controller, posMs);
        } else if (paramTypes.length == 2 && paramTypes[0] == String.class && paramTypes[1] == long.class) {
            seekMethod.invoke(controller, pkg, posMs);
        } else if (paramTypes.length == 3 && paramTypes[0] == String.class && paramTypes[2] == long.class) {
            seekMethod.invoke(controller, pkg, null, posMs);
        } else {
            throw new RuntimeException("Unknown params for seekTo: " + java.util.Arrays.toString(paramTypes));
        }
    }
}

