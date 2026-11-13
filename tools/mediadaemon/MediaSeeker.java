import java.lang.reflect.Method;
import java.util.List;
import android.os.IBinder;
import android.os.SystemClock;
import android.media.session.PlaybackState;
import android.media.MediaMetadata;

public class MediaSeeker {
    public static void main(String[] args) {
        if (args.length < 1) {
            System.out.println("Usage: MediaSeeker <seek_pos|+offset|-offset|get|pause|play|next|prev> [pkg_pattern]");
            return;
        }

        String command = args[0];
        String pkgPattern = args.length > 1 ? args[1] : "smarttube";

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
                System.out.println("NO_SESSIONS");
                return;
            }

            Class<?> iscStub = Class.forName("android.media.session.ISessionController$Stub");
            Method asController = iscStub.getMethod("asInterface", IBinder.class);

            for (Object b : binders) {
                Object controller = asController.invoke(null, (IBinder) b);
                Method getPkg = controller.getClass().getMethod("getPackageName");
                String pkg = (String) getPkg.invoke(controller);

                if (pkg != null && (pkgPattern.isEmpty() || pkg.contains(pkgPattern))) {
                    handleController(controller, pkg, command);
                    return;
                }
            }
            System.out.println("PKG_NOT_FOUND:" + pkgPattern);
        } catch (Exception e) {
            System.out.println("ERROR:" + e.getMessage());
            e.printStackTrace();
        }
    }

    private static void handleController(Object controller, String pkg, String command) throws Exception {
        Method getPlaybackStateMethod = controller.getClass().getMethod("getPlaybackState");
        PlaybackState pbState = (PlaybackState) getPlaybackStateMethod.invoke(controller);

        long livePos = 0;
        int state = 0;
        if (pbState != null) {
            state = pbState.getState();
            livePos = pbState.getPosition();
            if (state == PlaybackState.STATE_PLAYING) {
                long diff = SystemClock.elapsedRealtime() - pbState.getLastPositionUpdateTime();
                if (diff > 0) {
                    livePos += (long) (diff * pbState.getPlaybackSpeed());
                }
            }
        }

        if ("get".equalsIgnoreCase(command)) {
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

            System.out.println("STATUS:" + pkg + ":state=" + state + ":pos=" + livePos + ":duration=" + duration + ":title=" + (title == null ? "" : title));
            return;
        }

        if ("pause".equalsIgnoreCase(command)) {
            invokeTransport(controller, pkg, "pause");
            System.out.println("PAUSE_OK:" + pkg);
            return;
        }

        if ("play".equalsIgnoreCase(command) || "resume".equalsIgnoreCase(command)) {
            invokeTransport(controller, pkg, "play");
            System.out.println("PLAY_OK:" + pkg);
            return;
        }

        if ("next".equalsIgnoreCase(command)) {
            invokeTransport(controller, pkg, "next");
            System.out.println("NEXT_OK:" + pkg);
            return;
        }

        if ("prev".equalsIgnoreCase(command) || "previous".equalsIgnoreCase(command)) {
            invokeTransport(controller, pkg, "previous");
            System.out.println("PREV_OK:" + pkg);
            return;
        }

        long targetMs;
        if (command.startsWith("+")) {
            long delta = Long.parseLong(command.substring(1));
            targetMs = livePos + delta;
        } else if (command.startsWith("-")) {
            long delta = Long.parseLong(command.substring(1));
            targetMs = Math.max(0, livePos - delta);
        } else {
            targetMs = Math.max(0, Long.parseLong(command));
        }

        invokeSeek(controller, pkg, targetMs);
        System.out.println("SEEK_OK:" + pkg + ":from=" + livePos + ":to=" + targetMs);
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

